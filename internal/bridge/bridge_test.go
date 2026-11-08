package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sujoshua/agy-gateway/internal/openai"
)

type sink struct {
	calls   chan *Call
	expired chan string
}

func newSink() *sink { return &sink{calls: make(chan *Call, 4), expired: make(chan string, 4)} }

func (s *sink) ToolCall(c *Call)                   { s.calls <- c }
func (s *sink) CallExpired(_ *Call, reason string) { s.expired <- reason }

var echoTool = openai.Tool{Type: "function", Function: openai.FunctionDef{
	Name: "echo", Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
}}

type bearer struct{ secret string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(r)
}

// connect opens an MCP client session to a tenant, as the runtime does.
func connect(t *testing.T, url, secret string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer{secret}}, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func setup(t *testing.T, timeout time.Duration) (*Bridge, *Tenant, *sink, string) {
	t.Helper()
	b := New(Options{ResultTimeout: timeout, Keepalive: 20 * time.Millisecond})
	s := newSink()
	tn := b.Register([]openai.Tool{echoTool}, s)
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return b, tn, s, srv.URL + "/mcp/" + tn.ID
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatalf("empty result: %+v", r)
	}
	c, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("not text: %T", r.Content[0])
	}
	return c.Text
}

func object(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

func TestAuth(t *testing.T) {
	_, tn, _, url := setup(t, time.Second)
	for _, c := range []struct {
		url, secret string
		want        int
	}{
		{url, "wrong", http.StatusUnauthorized},
		{strings.TrimSuffix(url, tn.ID) + "nope", tn.Secret, http.StatusNotFound},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, c.url, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+c.secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s: %d, want %d", c.url, resp.StatusCode, c.want)
		}
	}
}

func TestCallRoundTrip(t *testing.T) {
	_, tn, s, url := setup(t, 5*time.Second)
	cs := connect(t, url, tn.Secret)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("tools: %v %v", tools, err)
	}
	go func() {
		c := <-s.calls
		if c.Args != `{"x":"hi"}` || !strings.HasPrefix(c.ID, "call_") {
			t.Errorf("call: %+v", c)
		}
		time.Sleep(60 * time.Millisecond) // long enough for keepalives
		c.Resolve(Result{Content: "hi back"})
		if c.Resolve(Result{Content: "again"}) {
			t.Error("a call resolved twice")
		}
	}()
	params := &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"x": "hi"}}
	params.SetProgressToken("p1")
	res, err := cs.CallTool(t.Context(), params)
	if err != nil || res.IsError || text(t, res) != "hi back" {
		t.Fatalf("result: %v %v", res, err)
	}
}

func TestTimeout(t *testing.T) {
	_, tn, s, url := setup(t, 50*time.Millisecond)
	cs := connect(t, url, tn.Secret)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(text(t, res), "in time") {
		t.Fatalf("result: %v %v", res, err)
	}
	if c := <-s.calls; !c.Settled() {
		t.Fatal("timed-out call still open")
	}
	if r := <-s.expired; r != "timeout" {
		t.Fatalf("expired reason %q", r)
	}
}

// TestRemoveReleasesCalls: removing a tenant settles calls in flight
// instead of waiting for them.
func TestRemoveReleasesCalls(t *testing.T) {
	b, tn, s, url := setup(t, time.Minute)
	cs := connect(t, url, tn.Secret)
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := cs.CallTool(context.WithoutCancel(t.Context()), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
		done <- res
	}()
	<-s.calls
	removed := make(chan struct{})
	go func() { b.Remove(tn); close(removed) }()
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove blocked on an in-flight call")
	}
	if b.Len() != 0 {
		t.Fatal("tenant still registered")
	}
	select {
	case res := <-done:
		if res != nil && (!res.IsError || !strings.Contains(text(t, res), "closed")) {
			t.Fatalf("result: %v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call never returned")
	}
}

func TestCompactSchema(t *testing.T) {
	if s := CompactSchema(nil, 0); s["type"] != "object" {
		t.Fatalf("empty schema: %v", s)
	}
	raw := json.RawMessage(`{"properties":{"title":{"type":"string","description":"` + strings.Repeat("d", 500) + `"},"n":{"type":"integer","examples":[1,2]}},"description":"top"}`)
	full := CompactSchema(raw, 0)
	if full["type"] != "object" || size(full) < 500 {
		t.Fatalf("no budget must keep the schema: %v", full)
	}
	small := CompactSchema(raw, 200)
	props := object(t, small["properties"])
	if _, ok := props["title"]; !ok {
		t.Fatal("a property named like a keyword was dropped")
	}
	if _, ok := object(t, props["title"])["description"]; ok {
		t.Fatal("nested description kept over budget")
	}
	if _, ok := object(t, props["n"])["examples"]; ok {
		t.Fatal("examples kept over budget")
	}
	if small["description"] != "top" {
		t.Fatal("the top-level description must stay")
	}
}

func TestCanonicalArgs(t *testing.T) {
	for in, want := range map[string]string{"": "{}", "null": "{}", "not json": "{}", `{ "b":1, "a":2 }`: `{"a":2,"b":1}`} {
		if got := canonicalArgs(json.RawMessage(in)); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
}
