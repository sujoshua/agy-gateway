// Package acptest is a scripted ACP agent standing in for the Antigravity
// runtime in tests. It speaks real ACP over in-memory pipes and calls the
// gateway's tool bridge over real MCP Streamable HTTP, the way the runtime
// does.
package acptest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sujoshua/agy-gateway/internal/acp"
)

// PromptFunc scripts one session/prompt turn.
type PromptFunc func(t *Turn) (sdk.StopReason, error)

// Agent is a fake ACP runtime. Set Prompt before the gateway sends prompts.
type Agent struct {
	// MCPServerName is the server name the agent stamps on tool call
	// metadata, as the runtime does; it must match the gateway's.
	MCPServerName string

	mu       sync.Mutex
	prompt   PromptFunc
	conn     *sdk.AgentSideConnection
	kill     func()
	starts   int
	nextID   int
	sessions map[string]*Session
	closed   []string
	resumed  []string
	calls    atomic.Int64
}

// Session is the agent's view of one ACP session.
type Session struct {
	ID         string
	SystemText string
	Model      string
	Level      string
	Prompts    [][]sdk.ContentBlock

	servers []sdk.McpServer
	cancel  context.CancelFunc
	mcp     *mcp.ClientSession
}

// New returns an agent whose turns reply "ok".
func New() *Agent {
	a := &Agent{MCPServerName: "client", sessions: map[string]*Session{}}
	a.prompt = func(t *Turn) (sdk.StopReason, error) {
		return sdk.StopReasonEndTurn, t.Say("ok")
	}
	return a
}

// OnPrompt replaces the turn script.
func (a *Agent) OnPrompt(f PromptFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prompt = f
}

// Starter launches the agent on fresh pipes, like starting the process.
func (a *Agent) Starter() acp.Starter {
	return func(context.Context) (*acp.Process, error) {
		clientR, agentW := io.Pipe()
		agentR, clientW := io.Pipe()
		conn := sdk.NewAgentSideConnection(a, agentW, agentR)
		done := make(chan struct{})
		var once sync.Once
		kill := func() {
			once.Do(func() {
				_ = clientW.Close()
				_ = agentW.Close()
				_ = clientR.Close()
				_ = agentR.Close()
				close(done)
			})
		}
		a.mu.Lock()
		a.conn, a.kill = conn, kill
		a.starts++
		a.mu.Unlock()
		return &acp.Process{Stdin: clientW, Stdout: clientR, Done: done, Kill: kill}, nil
	}
}

// Crash kills the running agent process.
func (a *Agent) Crash() {
	a.mu.Lock()
	kill := a.kill
	a.mu.Unlock()
	if kill != nil {
		kill()
	}
}

// Starts counts process starts.
func (a *Agent) Starts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts
}

// Session returns a session by id.
func (a *Agent) Session(id string) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// Sessions returns the ids of sessions that are open.
func (a *Agent) Sessions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id := range a.sessions {
		ids = append(ids, id)
	}
	// Creation order: "sess-N" with N increasing.
	slices.SortFunc(ids, func(x, y string) int { return cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y)) })
	return ids
}

// Closed returns the ids passed to session/close.
func (a *Agent) Closed() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.closed...)
}

// Resumed returns the ids passed to session/resume.
func (a *Agent) Resumed() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.resumed...)
}

// PromptsOf returns a copy of the prompts a session received.
func (a *Agent) PromptsOf(id string) [][]sdk.ContentBlock {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		return append([][]sdk.ContentBlock(nil), s.Prompts...)
	}
	return nil
}

// ToolCalls counts MCP tools/call requests the agent made.
func (a *Agent) ToolCalls() int { return int(a.calls.Load()) }

var _ sdk.Agent = (*Agent)(nil)

// Initialize implements sdk.Agent.
func (*Agent) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion:   sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{McpCapabilities: sdk.McpCapabilities{Http: true}},
		AgentInfo:         &sdk.Implementation{Name: "acptest", Version: "1"},
	}, nil
}

// Authenticate implements sdk.Agent.
func (*Agent) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}

// Logout implements sdk.Agent.
func (*Agent) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, errors.New("acptest: logout is never expected")
}

// ListSessions implements sdk.Agent.
func (*Agent) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}

// SetSessionMode implements sdk.Agent.
func (*Agent) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, nil
}

func configOptions() []sdk.SessionConfigOption {
	sel := func(id string, cat sdk.SessionConfigOptionCategory, values ...string) sdk.SessionConfigOption {
		opts := sdk.SessionConfigSelectOptionsUngrouped{}
		for _, v := range values {
			opts = append(opts, sdk.SessionConfigSelectOption{Name: v, Value: sdk.SessionConfigValueId(v)})
		}
		o := sdk.NewSessionConfigOptionSelect(sdk.SessionConfigValueId(values[0]), sdk.SessionConfigSelectOptions{Ungrouped: &opts})
		o.Select.Id, o.Select.Name, o.Select.Category = sdk.SessionConfigId(id), id, &cat
		return o
	}
	return []sdk.SessionConfigOption{
		sel("model", sdk.SessionConfigOptionCategoryModel, "mock-1"),
		sel("thinking_level", sdk.SessionConfigOptionCategoryThoughtLevel, "low", "high"),
	}
}

// NewSession implements sdk.Agent.
func (a *Agent) NewSession(_ context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := fmt.Sprintf("sess-%d", a.nextID)
	s := &Session{ID: id, servers: p.McpServers}
	if sys, ok := dig(p.Meta, "antigravity", "systemPrompt", "append").(string); ok {
		s.SystemText = sys
	}
	a.sessions[id] = s
	return sdk.NewSessionResponse{
		SessionId:     sdk.SessionId(id),
		ConfigOptions: configOptions(),
		Meta:          map[string]any{"antigravity": map[string]any{"tier": map[string]any{"name": "Mock Tier"}}},
	}, nil
}

// ResumeSession implements sdk.Agent.
func (a *Agent) ResumeSession(_ context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[string(p.SessionId)]
	if s == nil {
		return sdk.ResumeSessionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown session"})
	}
	a.resumed = append(a.resumed, s.ID)
	s.servers = p.McpServers
	s.mcp = nil // the old MCP connection belonged to the dead process
	return sdk.ResumeSessionResponse{ConfigOptions: configOptions()}, nil
}

// CloseSession implements sdk.Agent.
func (a *Agent) CloseSession(_ context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[string(p.SessionId)]; s != nil {
		if s.mcp != nil {
			_ = s.mcp.Close()
		}
		delete(a.sessions, s.ID)
	}
	a.closed = append(a.closed, string(p.SessionId))
	return sdk.CloseSessionResponse{}, nil
}

// SetSessionConfigOption implements sdk.Agent.
func (a *Agent) SetSessionConfigOption(_ context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	v := p.ValueId
	if v == nil {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "want a value id"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[string(v.SessionId)]
	if s == nil {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown session"})
	}
	switch v.ConfigId {
	case "model":
		s.Model = string(v.Value)
	case "thinking_level":
		s.Level = string(v.Value)
	}
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: configOptions()}, nil
}

// Cancel implements sdk.Agent.
func (a *Agent) Cancel(_ context.Context, p sdk.CancelNotification) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[string(p.SessionId)]; s != nil && s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Prompt implements sdk.Agent.
func (a *Agent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	a.mu.Lock()
	s := a.sessions[string(p.SessionId)]
	if s == nil {
		a.mu.Unlock()
		return sdk.PromptResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown session"})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.cancel = cancel
	s.Prompts = append(s.Prompts, p.Prompt)
	f, conn := a.prompt, a.conn
	a.mu.Unlock()

	t := &Turn{Ctx: ctx, Session: s, Prompt: p.Prompt, agent: a, conn: conn}
	stop, err := f(t)
	// A canceled turn ends with the canceled stop reason, as ACP requires,
	// whatever the script was doing when it noticed.
	if ctx.Err() != nil {
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled, Usage: t.usage()}, nil
	}
	if err != nil {
		return sdk.PromptResponse{}, err
	}
	return sdk.PromptResponse{StopReason: stop, Usage: t.usage()}, nil
}

// Turn is one running prompt, handed to the PromptFunc.
type Turn struct {
	Ctx     context.Context
	Session *Session
	Prompt  []sdk.ContentBlock

	agent  *Agent
	conn   *sdk.AgentSideConnection
	n      atomic.Int64
	output atomic.Int64
	noUse  atomic.Bool
}

// Text joins the prompt's text blocks.
func (t *Turn) Text() string {
	var b strings.Builder
	for _, c := range t.Prompt {
		if c.Text != nil {
			b.WriteString(c.Text.Text)
		}
	}
	return b.String()
}

// NoUsage makes the turn report no token usage.
func (t *Turn) NoUsage() { t.noUse.Store(true) }

func (t *Turn) usage() *sdk.Usage {
	if t.noUse.Load() {
		return nil
	}
	thought := 3
	cached := 5
	out := int(t.output.Load())
	return &sdk.Usage{InputTokens: 100, OutputTokens: out, ThoughtTokens: &thought, CachedReadTokens: &cached, TotalTokens: 100 + out + thought}
}

func (t *Turn) update(u sdk.SessionUpdate) error {
	return t.conn.SessionUpdate(t.Ctx, sdk.SessionNotification{SessionId: sdk.SessionId(t.Session.ID), Update: u})
}

// Say streams an agent message chunk.
func (t *Turn) Say(text string) error {
	t.output.Add(int64(len(strings.Fields(text))))
	return t.update(sdk.UpdateAgentMessageText(text))
}

// Think streams a thought chunk.
func (t *Turn) Think(text string) error { return t.update(sdk.UpdateAgentThoughtText(text)) }

// ToolResult is what a tool call returned to the agent.
type ToolResult struct {
	Text    string
	IsError bool
}

// Call runs a tool of the gateway's MCP server the way the runtime does:
// a tool_call update, a permission request, then MCP tools/call.
func (t *Turn) Call(name string, args map[string]any) (ToolResult, error) {
	id := sdk.ToolCallId(fmt.Sprintf("tc-%d", t.n.Add(1)))
	meta := map[string]any{"is_mcp_tool_call": true, "mcp": map[string]any{"server": t.agent.MCPServerName, "tool": name}}
	if err := t.update(sdk.StartToolCall(id, t.agent.MCPServerName+"_"+name, sdk.WithStartRawInput(args))); err != nil {
		return ToolResult{}, err
	}
	title := t.agent.MCPServerName + "_" + name
	perm, err := t.conn.RequestPermission(t.Ctx, sdk.RequestPermissionRequest{
		SessionId: sdk.SessionId(t.Session.ID),
		ToolCall:  sdk.ToolCallUpdate{ToolCallId: id, Title: &title, Meta: meta},
		Options: []sdk.PermissionOption{
			{OptionId: "allow", Kind: sdk.PermissionOptionKindAllowOnce, Name: "Allow"},
			{OptionId: "reject", Kind: sdk.PermissionOptionKindRejectOnce, Name: "Reject"},
		},
	})
	if err != nil {
		return ToolResult{}, err
	}
	if perm.Outcome.Selected == nil || perm.Outcome.Selected.OptionId != "allow" {
		return ToolResult{}, errors.New("acptest: permission denied")
	}
	cs, err := t.mcpSession()
	if err != nil {
		return ToolResult{}, err
	}
	t.agent.calls.Add(1)
	params := &mcp.CallToolParams{Name: name, Arguments: args}
	params.SetProgressToken(string(id))
	res, err := cs.CallTool(t.Ctx, params)
	if err != nil {
		return ToolResult{}, err
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return ToolResult{Text: b.String(), IsError: res.IsError}, nil
}

// RequestForeignPermission asks to run a runtime-internal tool; the
// gateway must refuse it.
func (t *Turn) RequestForeignPermission(title string) (bool, error) {
	perm, err := t.conn.RequestPermission(t.Ctx, sdk.RequestPermissionRequest{
		SessionId: sdk.SessionId(t.Session.ID),
		ToolCall:  sdk.ToolCallUpdate{ToolCallId: "foreign", Title: &title},
		Options: []sdk.PermissionOption{
			{OptionId: "allow", Kind: sdk.PermissionOptionKindAllowOnce, Name: "Allow"},
			{OptionId: "reject", Kind: sdk.PermissionOptionKindRejectOnce, Name: "Reject"},
		},
	})
	if err != nil {
		return false, err
	}
	return perm.Outcome.Selected != nil && perm.Outcome.Selected.OptionId == "allow", nil
}

func (t *Turn) mcpSession() (*mcp.ClientSession, error) {
	a := t.agent
	a.mu.Lock()
	s := t.Session
	if s.mcp != nil {
		cs := s.mcp
		a.mu.Unlock()
		return cs, nil
	}
	var srv *sdk.McpServerHttpInline
	for _, m := range s.servers {
		if m.Http != nil && m.Http.Name == a.MCPServerName {
			srv = m.Http
		}
	}
	a.mu.Unlock()
	if srv == nil {
		return nil, errors.New("acptest: the session has no gateway MCP server")
	}
	hdr := http.Header{}
	for _, h := range srv.Headers {
		hdr.Set(h.Name, h.Value)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "acptest", Version: "1"}, nil)
	cs, err := client.Connect(context.WithoutCancel(t.Ctx), &mcp.StreamableClientTransport{
		Endpoint:   srv.Url,
		HTTPClient: &http.Client{Transport: headerTransport{hdr}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	s.mcp = cs
	a.mu.Unlock()
	return cs, nil
}

type headerTransport struct{ h http.Header }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	maps.Copy(r.Header, h.h)
	return http.DefaultTransport.RoundTrip(r)
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}
