// Package bridge exposes each gateway session's OpenAI tools to the runtime
// as a native MCP server over Streamable HTTP. A tools/call is held open
// until the OpenAI client posts the matching tool result, so function calling
// stays native end to end.
package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sujoshua/agy-gateway/internal/openai"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Result is the outcome of one tool call.
type Result struct {
	Content string
	IsError bool
}

// Call is one tool invocation by the model, waiting for its result.
type Call struct {
	ID   string
	Name string
	Args string // canonical JSON object

	result  chan Result
	settled atomic.Bool
}

// Resolve delivers the result. It returns false when the call was already
// settled (answered, timed out, or canceled by the runtime).
func (c *Call) Resolve(r Result) bool {
	if !c.settled.CompareAndSwap(false, true) {
		return false
	}
	c.result <- r
	return true
}

// Settled reports whether the call no longer accepts a result.
func (c *Call) Settled() bool { return c.settled.Load() }

// NewCall builds a call; exported for tests of the session layer.
func NewCall(name, args string) *Call {
	return &Call{ID: NewCallID(), Name: name, Args: args, result: make(chan Result, 1)}
}

// Sink receives tool calls and expiry notices for one tenant.
type Sink interface {
	// ToolCall is called when the model invokes a tool.
	ToolCall(c *Call)
	// CallExpired is called when a call settles without a client result.
	CallExpired(c *Call, reason string)
}

// Options configures a Bridge.
type Options struct {
	ResultTimeout time.Duration
	Keepalive     time.Duration
	SchemaBudget  int
	Logger        *slog.Logger
}

// Bridge routes /mcp/{tenant} to per-session MCP servers.
type Bridge struct {
	opt     Options
	log     *slog.Logger
	mu      sync.RWMutex
	tenants map[string]*Tenant
}

// Tenant is the MCP server of one gateway session.
type Tenant struct {
	ID      string
	Secret  string
	server  *mcp.Server
	handler http.Handler
	sink    Sink
	closed  atomic.Bool

	mu    sync.Mutex
	calls map[*Call]struct{} // in flight, settled by Remove
}

// New creates a Bridge.
func New(opt Options) *Bridge {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Keepalive <= 0 {
		opt.Keepalive = 15 * time.Second
	}
	return &Bridge{opt: opt, log: opt.Logger, tenants: map[string]*Tenant{}}
}

// Register creates a tenant exposing tools; calls go to sink.
func (b *Bridge) Register(tools []openai.Tool, sink Sink) *Tenant {
	t := &Tenant{ID: randHex(16), Secret: randHex(32), sink: sink, calls: map[*Call]struct{}{}}
	t.server = mcp.NewServer(&mcp.Implementation{Name: "agy-gateway-tools", Version: "1"}, &mcp.ServerOptions{
		Logger: slog.New(warnOnly{b.log.Handler()}),
	})
	for _, tool := range tools {
		t.server.AddTool(&mcp.Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: CompactSchema(tool.Function.Parameters, b.opt.SchemaBudget),
		}, b.handle(t))
	}
	t.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return t.server }, &mcp.StreamableHTTPOptions{
		Logger: slog.New(warnOnly{b.log.Handler()}),
	})
	b.mu.Lock()
	b.tenants[t.ID] = t
	b.mu.Unlock()
	return t
}

// Remove unregisters a tenant and closes its MCP sessions. Calls still in
// flight are settled as canceled.
func (b *Bridge) Remove(t *Tenant) {
	if t == nil || !t.closed.CompareAndSwap(false, true) {
		return
	}
	b.mu.Lock()
	delete(b.tenants, t.ID)
	b.mu.Unlock()
	// Closing an MCP session waits for its handlers, so release them first.
	t.mu.Lock()
	calls := make([]*Call, 0, len(t.calls))
	for c := range t.calls {
		calls = append(calls, c)
	}
	t.mu.Unlock()
	for _, c := range calls {
		c.Resolve(Result{Content: "The conversation was closed before the client returned a result.", IsError: true})
	}
	for ss := range t.server.Sessions() {
		_ = ss.Close()
	}
}

// Len returns the number of live tenants.
func (b *Bridge) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.tenants)
}

// ServeHTTP authenticates the tenant and hands off to its MCP handler.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	if !ok || id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	b.mu.RLock()
	t := b.tenants[id]
	b.mu.RUnlock()
	if t == nil {
		http.NotFound(w, r)
		return
	}
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(t.Secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	t.handler.ServeHTTP(w, r)
}

func (b *Bridge) handle(t *Tenant) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		call := NewCall(req.Params.Name, canonicalArgs(req.Params.Arguments))
		token := req.Params.GetProgressToken()
		var progress float64
		notify := func(msg string) {
			if token == nil {
				return
			}
			progress++
			// Each notification writes bytes on the open SSE response, which
			// keeps the runtime's HTTP client from timing out the call.
			err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: progress, Message: msg})
			if err != nil {
				b.log.Debug("progress notification failed", slog.Any("err", err))
			}
		}
		notify("waiting for the client to run " + call.Name)
		if !t.track(call) {
			return errorResult("the conversation was closed"), nil
		}
		defer t.untrack(call)
		t.sink.ToolCall(call)

		timeout := time.NewTimer(b.opt.ResultTimeout)
		defer timeout.Stop()
		tick := time.NewTicker(b.opt.Keepalive)
		defer tick.Stop()
		for {
			select {
			case r := <-call.result:
				res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: r.Content}}, IsError: r.IsError}
				return res, nil
			case <-tick.C:
				notify("still waiting for the client")
			case <-timeout.C:
				if call.Resolve(Result{Content: "The client did not return a result for this tool call in time.", IsError: true}) {
					t.sink.CallExpired(call, "timeout")
				}
			case <-ctx.Done():
				if call.Resolve(Result{Content: "canceled", IsError: true}) {
					t.sink.CallExpired(call, "canceled")
				}
				return nil, ctx.Err()
			}
		}
	}
}

// track records an in-flight call; it fails once the tenant is closed.
func (t *Tenant) track(c *Call) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed.Load() {
		return false
	}
	t.calls[c] = struct{}{}
	return true
}

func (t *Tenant) untrack(c *Call) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.calls, c)
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}

// canonicalArgs re-encodes arguments as compact JSON, defaulting to {}.
func canonicalArgs(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// CompactSchema returns the tool input schema, dropping nested
// documentation keys when it is larger than budget bytes. The compaction and
// the default budget of 1350 bytes follow the dsh-llm-antigravity-acp plugin
// (https://github.com/lili-luo/dsh-plugin-bundle), which reports that the
// runtime rejects or truncates very large tool schemas. That limit has not
// been measured here; the runtime's own run_workflow schema is 1640 bytes.
func CompactSchema(raw json.RawMessage, budget int) map[string]any {
	var schema map[string]any
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if budget <= 0 || size(schema) <= budget {
		return schema
	}
	for _, key := range []string{"examples", "$comment", "title", "default", "description"} {
		stripNested(schema, key, true)
		if size(schema) <= budget {
			break
		}
	}
	return schema
}

func size(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func stripNested(v any, key string, top bool) {
	switch x := v.(type) {
	case map[string]any:
		if !top {
			delete(x, key)
		}
		for k, child := range x {
			// Inside "properties" the map keys are property names, not keywords.
			if k == "properties" {
				if props, ok := child.(map[string]any); ok {
					for _, p := range props {
						stripNested(p, key, false)
					}
					continue
				}
			}
			stripNested(child, key, false)
		}
	case []any:
		for _, child := range x {
			stripNested(child, key, false)
		}
	}
}

// warnOnly drops the MCP SDK's per-connection info logs.
type warnOnly struct{ slog.Handler }

func (h warnOnly) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

func (h warnOnly) WithAttrs(a []slog.Attr) slog.Handler { return warnOnly{h.Handler.WithAttrs(a)} }

func (h warnOnly) WithGroup(n string) slog.Handler { return warnOnly{h.Handler.WithGroup(n)} }

// NewCallID returns an OpenAI-style tool call id.
func NewCallID() string { return "call_" + randHex(12) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("crypto/rand failed: " + err.Error()))
	}
	return hex.EncodeToString(b)
}
