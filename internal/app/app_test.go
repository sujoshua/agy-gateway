package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/acptest"
	"github.com/sujoshua/agy-gateway/internal/app"
	"github.com/sujoshua/agy-gateway/internal/config"
)

const key = "test-key"

type env struct {
	t     *testing.T
	agent *acptest.Agent
	app   *app.App
	url   string
}

func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	e := startEnv(t, mutate, acptest.New())
	e.waitReady()
	return e
}

// startEnv starts a gateway on agent without waiting for it to be ready.
func startEnv(t *testing.T, mutate func(*config.Config), agent *acptest.Agent) *env {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(func(k string) string {
		switch k {
		case "AGY_GW_API_KEYS":
			return key + ",other-key"
		case "AGY_GW_WORKDIR":
			return dir + "/work"
		case "AGY_GW_RUNTIME_HOME":
			return dir + "/home"
		case "AGY_GW_TOOL_BATCH_WINDOW":
			return "50ms"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
	}
	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, cfg, log, "test", agent.Starter())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Run(ctx) }()
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
	})
	return &env{t: t, agent: agent, app: a, url: srv.URL}
}

func (e *env) waitReady() {
	e.t.Helper()
	eventually(e.t, 5*time.Second, "runtime ready", func() bool {
		ok, _ := e.app.Runtime.Ready()
		return ok
	})
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) do(method, path, apiKey string, body any) *http.Response {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			r = strings.NewReader(s)
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				e.t.Fatal(err)
			}
			r = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.url+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

// chat posts a non-streaming request and decodes the JSON reply.
func (e *env) chat(body map[string]any) (status int, reply map[string]any) {
	e.t.Helper()
	return e.chatAs(key, body)
}

func (e *env) chatAs(apiKey string, body map[string]any) (status int, reply map[string]any) {
	e.t.Helper()
	resp := e.do(http.MethodPost, "/v1/chat/completions", apiKey, body)
	defer closeBody(resp)
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatalf("decoding reply (status %d): %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out
}

type sse struct {
	status int
	chunks []map[string]any
	done   bool
	errors []map[string]any
}

// stream posts a streaming request and collects the SSE events.
func (e *env) stream(body map[string]any) sse {
	e.t.Helper()
	body["stream"] = true
	resp := e.do(http.MethodPost, "/v1/chat/completions", key, body)
	defer closeBody(resp)
	out := sse{status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		out.errors = append(out.errors, v)
		return out
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		e.t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			out.done = true
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			e.t.Fatalf("bad SSE data %q: %v", data, err)
		}
		if _, isErr := v["error"]; isErr {
			out.errors = append(out.errors, v)
			continue
		}
		out.chunks = append(out.chunks, v)
	}
	return out
}

// assemble merges stream chunks into content, tool calls, finish reason and
// usage, the way OpenAI clients do.
func (s sse) assemble(t *testing.T) (content string, calls []map[string]any, finish string, usage map[string]any) {
	t.Helper()
	byIndex := map[int]map[string]any{}
	for _, c := range s.chunks {
		if u, ok := c["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := c["choices"].([]any)
		for _, ch := range choices {
			ch := as[map[string]any](t, ch)
			if f, ok := ch["finish_reason"].(string); ok {
				finish = f
			}
			delta, _ := ch["delta"].(map[string]any)
			if txt, ok := delta["content"].(string); ok {
				content += txt
			}
			tcs, _ := delta["tool_calls"].([]any)
			for _, tc := range tcs {
				tc := as[map[string]any](t, tc)
				i := int(as[float64](t, tc["index"]))
				fn := as[map[string]any](t, tc["function"])
				cur, ok := byIndex[i]
				if !ok {
					if tc["id"] == nil || fn["name"] == nil {
						t.Fatalf("first chunk of tool call %d lacks id or name: %v", i, tc)
					}
					cur = map[string]any{"id": tc["id"], "name": fn["name"], "arguments": ""}
					byIndex[i] = cur
				}
				if a, ok := fn["arguments"].(string); ok {
					cur["arguments"] = as[string](t, cur["arguments"]) + a
				}
			}
		}
	}
	for i := range len(byIndex) {
		c, ok := byIndex[i]
		if !ok {
			t.Fatalf("tool call indexes are not dense: %v", byIndex)
		}
		calls = append(calls, c)
	}
	return content, calls, finish, usage
}

var weatherTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_weather",
		"description": "Get the weather for a city.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
	},
}

func user(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

func toolResult(id, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

func message(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("want one choice: %v", resp)
	}
	return as[map[string]any](t, as[map[string]any](t, choices[0])["message"])
}

func finish(t *testing.T, resp map[string]any) string {
	t.Helper()
	choices := as[[]any](t, resp["choices"])
	if len(choices) != 1 {
		t.Fatalf("want one choice: %v", resp)
	}
	return as[string](t, as[map[string]any](t, choices[0])["finish_reason"])
}

func toolCalls(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	raw, _ := message(t, resp)["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, as[map[string]any](t, r))
	}
	return out
}

func callArgs(t *testing.T, call map[string]any) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(as[string](t, as[map[string]any](t, call["function"])["arguments"])), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v", err)
	}
	return args
}

// as asserts the type of a decoded JSON value.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	x, ok := v.(T)
	if !ok {
		t.Fatalf("want %T, got %T: %v", x, v, v)
	}
	return x
}

func closeBody(resp *http.Response) { _ = resp.Body.Close() }

func promptText(blocks []sdk.ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		if c.Text != nil {
			b.WriteString(c.Text.Text)
		}
	}
	return b.String()
}

// weatherAgent calls get_weather for each city named in the prompt and
// answers with the results; other prompts are echoed.
func weatherAgent(t *acptest.Turn) (sdk.StopReason, error) {
	text := t.Text()
	var cities []string
	for _, c := range []string{"Paris", "Tokyo"} {
		if strings.Contains(text, "weather in") && strings.Contains(text, c) {
			cities = append(cities, c)
		}
	}
	if len(cities) == 0 {
		return sdk.StopReasonEndTurn, t.Say("echo: " + text)
	}
	var answers []string
	for _, c := range cities {
		r, err := t.Call("get_weather", map[string]any{"city": c})
		if err != nil {
			return "", err
		}
		answers = append(answers, fmt.Sprintf("%s=%s(error=%v)", c, r.Text, r.IsError))
	}
	return sdk.StopReasonEndTurn, t.Say("Weather: " + strings.Join(answers, "; "))
}

func TestHealthModelsAndAuth(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp := e.do(http.MethodGet, path, "", nil)
		closeBody(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	resp := e.do(http.MethodGet, "/v1/models", "", nil)
	closeBody(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("models without a key: %d", resp.StatusCode)
	}
	resp = e.do(http.MethodGet, "/v1/models", key, nil)
	defer closeBody(resp)
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"mock-1", "mock-1-low", "mock-1-high"}) {
		t.Fatalf("models: %v", ids)
	}
	resp = e.do(http.MethodPost, "/v1/responses", key, "{}")
	closeBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/responses: %d", resp.StatusCode)
	}
}

func TestInvalidRequests(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		name   string
		body   any
		status int
	}{
		{"bad json", "{", 400},
		{"no model", map[string]any{"messages": []any{user("hi")}}, 400},
		{"no messages", map[string]any{"model": "mock-1"}, 400},
		{"unknown model", map[string]any{"model": "gpt-4o", "messages": []any{user("hi")}}, 404},
		{"n=2", map[string]any{"model": "mock-1", "n": 2, "messages": []any{user("hi")}}, 400},
		{"logprobs", map[string]any{"model": "mock-1", "logprobs": true, "messages": []any{user("hi")}}, 400},
		{"temperature", map[string]any{"model": "mock-1", "temperature": 0.2, "messages": []any{user("hi")}}, 400},
		{"json schema", map[string]any{"model": "mock-1", "response_format": map[string]any{"type": "json_schema"}, "messages": []any{user("hi")}}, 400},
		{"bad role", map[string]any{"model": "mock-1", "messages": []any{map[string]any{"role": "robot", "content": "x"}}}, 400},
		{"stream_options without stream", map[string]any{"model": "mock-1", "stream_options": map[string]any{"include_usage": true}, "messages": []any{user("hi")}}, 400},
		{"orphan tool result", map[string]any{"model": "mock-1", "messages": []any{user("hi"), toolResult("call_x", "1")}}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := e.do(http.MethodPost, "/v1/chat/completions", key, c.body)
			defer closeBody(resp)
			var v map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&v)
			if resp.StatusCode != c.status || v["error"] == nil {
				t.Fatalf("want %d with an error body, got %d: %v", c.status, resp.StatusCode, v)
			}
		})
	}
	if len(e.agent.Sessions()) != 0 {
		t.Fatalf("invalid requests reached the runtime: %v", e.agent.Sessions())
	}
}

func TestIgnoreUnsupportedSampling(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.IgnoreUnsupported = true })
	status, resp := e.chat(map[string]any{"model": "mock-1", "temperature": 0.2, "max_tokens": 10, "messages": []any{user("hi")}})
	if status != 200 || message(t, resp)["content"] != "ok" {
		t.Fatalf("%d %v", status, resp)
	}
}

// TestToolLoop is the native function-calling round trip: the model's MCP
// call becomes an OpenAI tool call, the client's tool message becomes the
// MCP result, and the same runtime turn finishes the answer.
func TestToolLoop(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(weatherAgent)
	msgs := []any{
		map[string]any{"role": "system", "content": "Be brief."},
		map[string]any{"role": "developer", "content": "Use tools."},
		user("What is the weather in Paris?"),
	}
	status, first := e.chat(map[string]any{"model": "mock-1-low", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 {
		t.Fatalf("%d %v", status, first)
	}
	calls := toolCalls(t, first)
	if finish(t, first) != "tool_calls" || len(calls) != 1 {
		t.Fatalf("want one tool call: %v", first)
	}
	call := calls[0]
	id := as[string](t, call["id"])
	if !strings.HasPrefix(id, "call_") || call["type"] != "function" || as[map[string]any](t, call["function"])["name"] != "get_weather" {
		t.Fatalf("tool call: %v", call)
	}
	if callArgs(t, call)["city"] != "Paris" {
		t.Fatalf("arguments: %v", call)
	}
	if first["usage"] != nil {
		t.Fatalf("usage reported before the turn ended: %v", first["usage"])
	}

	msgs = append(msgs, message(t, first), toolResult(id, `{"temp_c":17}`))
	status, final := e.chat(map[string]any{"model": "mock-1-low", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 {
		t.Fatalf("%d %v", status, final)
	}
	if got := message(t, final)["content"]; got != `Weather: Paris={"temp_c":17}(error=false)` || finish(t, final) != "stop" {
		t.Fatalf("final: %v", final)
	}
	usage := as[map[string]any](t, final["usage"])
	if as[float64](t, usage["prompt_tokens"]) != 100 || as[float64](t, as[map[string]any](t, usage["completion_tokens_details"])["reasoning_tokens"]) != 3 {
		t.Fatalf("usage: %v", usage)
	}

	ids := e.agent.Sessions()
	if len(ids) != 1 {
		t.Fatalf("sessions: %v", ids)
	}
	s := e.agent.Session(ids[0])
	if s.SystemText != "Be brief.\n\nUse tools." && !strings.Contains(s.SystemText, "Be brief.") {
		t.Fatalf("system prompt: %q", s.SystemText)
	}
	if s.Model != "mock-1" || s.Level != "low" {
		t.Fatalf("model %q level %q", s.Model, s.Level)
	}
	if n := len(e.agent.PromptsOf(s.ID)); n != 1 {
		t.Fatalf("the tool result must continue the turn, not start a new prompt: %d prompts", n)
	}

	// Follow-up: same session, and only the new message is sent.
	msgs = append(msgs, message(t, final), user("thanks"))
	status, follow := e.chat(map[string]any{"model": "mock-1-low", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 || message(t, follow)["content"] != "echo: thanks" {
		t.Fatalf("follow-up: %d %v", status, follow)
	}
	prompts := e.agent.PromptsOf(s.ID)
	if len(prompts) != 2 || strings.Contains(promptText(prompts[1]), "Paris") {
		t.Fatalf("history was re-sent: %q", promptText(prompts[len(prompts)-1]))
	}
	if e.app.Sessions.Len() != 1 {
		t.Fatalf("gateway sessions: %d", e.app.Sessions.Len())
	}

	// The same result again is a duplicate.
	status, resp := e.chat(map[string]any{"model": "mock-1-low", "tools": []any{weatherTool}, "messages": msgs[:5]})
	if status != 400 || !strings.Contains(fmt.Sprint(resp["error"]), "already submitted") {
		t.Fatalf("duplicate result: %d %v", status, resp)
	}
}

func TestStreamingToolLoop(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if err := t.Think("pondering"); err != nil {
			return "", err
		}
		long := strings.Repeat("x", 200)
		r, err := t.Call("get_weather", map[string]any{"city": "Paris", "note": long})
		if err != nil {
			return "", err
		}
		for i := range 300 {
			if err := t.Say(fmt.Sprintf("w%d ", i)); err != nil {
				return "", err
			}
		}
		return sdk.StopReasonEndTurn, t.Say(r.Text)
	})
	body := func(msgs []any) map[string]any {
		return map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs, "stream_options": map[string]any{"include_usage": true}}
	}
	msgs := []any{user("weather?")}
	s := e.stream(body(msgs))
	if s.status != 200 || !s.done || len(s.errors) > 0 {
		t.Fatalf("stream: %+v", s)
	}
	_, calls, fin, usage := s.assemble(t)
	if fin != "tool_calls" || len(calls) != 1 || usage != nil {
		t.Fatalf("finish %q calls %v usage %v", fin, calls, usage)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(as[string](t, calls[0]["arguments"])), &args); err != nil || args["note"] != strings.Repeat("x", 200) {
		t.Fatalf("streamed arguments: %v %v", calls[0]["arguments"], err)
	}
	fragments := 0
	for _, c := range s.chunks {
		if strings.Contains(fmt.Sprint(c), "arguments") {
			fragments++
		}
	}
	if fragments < 3 {
		t.Fatalf("arguments were not streamed incrementally: %d chunks", fragments)
	}
	reasoning := false
	for _, c := range s.chunks {
		if strings.Contains(fmt.Sprint(c), "reasoning_content:pondering") {
			reasoning = true
		}
	}
	if !reasoning {
		t.Fatal("no reasoning_content chunk")
	}

	msgs = append(msgs,
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": calls[0]["id"], "type": "function", "function": map[string]any{"name": "get_weather", "arguments": calls[0]["arguments"]}}}},
		toolResult(as[string](t, calls[0]["id"]), "sunny"))
	s = e.stream(body(msgs))
	content, calls, fin, usage := s.assemble(t)
	if !s.done || fin != "stop" || len(calls) != 0 || !strings.HasSuffix(content, "w299 sunny") || !strings.HasPrefix(content, "w0 w1 ") {
		t.Fatalf("final stream: finish %q, %d calls, content ...%q", fin, len(calls), content[max(0, len(content)-40):])
	}
	if usage == nil || as[float64](t, usage["total_tokens"]) == 0 {
		t.Fatalf("usage: %v", usage)
	}
	last := s.chunks[len(s.chunks)-1]
	if ch, _ := last["choices"].([]any); len(ch) != 0 || last["usage"] == nil {
		t.Fatalf("the usage chunk must come last with empty choices: %v", last)
	}
}

func TestParallelAndConsecutiveCalls(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		// Two calls at once, then one more after both results.
		var wg sync.WaitGroup
		res := make([]acptest.ToolResult, 2)
		errs := make([]error, 2)
		for i, c := range []string{"Paris", "Tokyo"} {
			wg.Go(func() { res[i], errs[i] = t.Call("get_weather", map[string]any{"city": c}) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return "", err
		}
		third, err := t.Call("get_weather", map[string]any{"city": "Oslo"})
		if err != nil {
			return "", err
		}
		return sdk.StopReasonEndTurn, t.Say(res[0].Text + "," + res[1].Text + "," + third.Text)
	})
	tools := []any{weatherTool}
	msgs := []any{user("weather in Paris and Tokyo")}
	status, r1 := e.chat(map[string]any{"model": "mock-1", "tools": tools, "messages": msgs})
	calls := toolCalls(t, r1)
	if status != 200 || len(calls) != 2 || finish(t, r1) != "tool_calls" {
		t.Fatalf("want two parallel calls: %d %v", status, r1)
	}
	msgs = append(msgs, message(t, r1))
	for _, c := range calls {
		msgs = append(msgs, toolResult(as[string](t, c["id"]), as[string](t, callArgs(t, c)["city"])))
	}
	status, r2 := e.chat(map[string]any{"model": "mock-1", "tools": tools, "messages": msgs})
	calls = toolCalls(t, r2)
	if status != 200 || len(calls) != 1 || callArgs(t, calls[0])["city"] != "Oslo" {
		t.Fatalf("want the consecutive call: %d %v", status, r2)
	}
	msgs = append(msgs, message(t, r2), toolResult(as[string](t, calls[0]["id"]), "Oslo"))
	status, r3 := e.chat(map[string]any{"model": "mock-1", "tools": tools, "messages": msgs})
	got, _ := message(t, r3)["content"].(string)
	if status != 200 || got == "" || !strings.Contains(got, "Oslo") || !strings.Contains(got, "Paris") || !strings.Contains(got, "Tokyo") {
		t.Fatalf("final: %d %v", status, r3)
	}
	if e.agent.ToolCalls() != 3 || e.app.Sessions.Len() != 1 {
		t.Fatalf("tool calls %d, sessions %d", e.agent.ToolCalls(), e.app.Sessions.Len())
	}
}

// TestParallelToolCallsFalse gets concurrent calls one per response.
func TestParallelToolCallsFalse(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		var wg sync.WaitGroup
		res := make([]acptest.ToolResult, 2)
		for i, c := range []string{"Paris", "Tokyo"} {
			wg.Go(func() { res[i], _ = t.Call("get_weather", map[string]any{"city": c}) })
		}
		wg.Wait()
		return sdk.StopReasonEndTurn, t.Say(res[0].Text + "," + res[1].Text)
	})
	msgs := []any{user("both")}
	seen := map[string]bool{}
	for round := range 3 {
		status, r := e.chat(map[string]any{"model": "mock-1", "parallel_tool_calls": false, "tools": []any{weatherTool}, "messages": msgs})
		if status != 200 {
			t.Fatalf("round %d: %d %v", round, status, r)
		}
		if finish(t, r) == "stop" {
			if round != 2 || as[string](t, message(t, r)["content"]) != "Paris,Tokyo" {
				t.Fatalf("round %d final: %v", round, r)
			}
			return
		}
		calls := toolCalls(t, r)
		if len(calls) != 1 {
			t.Fatalf("round %d: want one call, got %d", round, len(calls))
		}
		city := as[string](t, callArgs(t, calls[0])["city"])
		seen[city] = true
		msgs = append(msgs, message(t, r), toolResult(as[string](t, calls[0]["id"]), city))
	}
	t.Fatalf("never finished; saw %v", seen)
}

// TestMissingResultIsReportedAsError answers only one of two calls.
func TestMissingResultIsReportedAsError(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(weatherAgent)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		var wg sync.WaitGroup
		res := make([]acptest.ToolResult, 2)
		for i, c := range []string{"Paris", "Tokyo"} {
			wg.Go(func() { res[i], _ = t.Call("get_weather", map[string]any{"city": c}) })
		}
		wg.Wait()
		return sdk.StopReasonEndTurn, t.Say(fmt.Sprintf("%v|%v", res[0].IsError, res[1].IsError))
	})
	msgs := []any{user("go")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	calls := toolCalls(t, r1)
	if len(calls) != 2 {
		t.Fatalf("calls: %v", r1)
	}
	msgs = append(msgs, message(t, r1), toolResult(as[string](t, calls[0]["id"]), "ok"))
	_, r2 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	got := as[string](t, message(t, r2)["content"])
	if got != "false|true" && got != "true|false" {
		t.Fatalf("one result must be an error: %q", got)
	}
}

// TestToolTimeoutThenLateResult lets a call time out in the bridge; the
// late result is then delivered as input of a new turn.
func TestToolTimeoutThenLateResult(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ToolResultTimeout = 200 * time.Millisecond })
	gaveUp := make(chan string, 1)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if strings.Contains(t.Text(), "late-result") {
			return sdk.StopReasonEndTurn, t.Say("got the late result")
		}
		r, err := t.Call("get_weather", map[string]any{"city": "Paris"})
		if err != nil {
			return "", err
		}
		gaveUp <- r.Text
		return sdk.StopReasonEndTurn, t.Say("the tool timed out")
	})
	msgs := []any{user("weather")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	calls := toolCalls(t, r1)
	if len(calls) != 1 {
		t.Fatalf("calls: %v", r1)
	}
	select {
	case txt := <-gaveUp:
		if !strings.Contains(txt, "did not return a result") {
			t.Fatalf("timeout result: %q", txt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge did not time the call out")
	}
	sid := e.agent.Sessions()[0]
	eventually(t, 5*time.Second, "turn end", func() bool { return len(e.agent.PromptsOf(sid)) == 1 })

	msgs = append(msgs, message(t, r1), toolResult(as[string](t, calls[0]["id"]), "late-result"))
	status, r2 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 || message(t, r2)["content"] != "got the late result" {
		t.Fatalf("late result: %d %v", status, r2)
	}
	prompts := e.agent.PromptsOf(sid)
	if len(prompts) != 2 || !strings.Contains(promptText(prompts[1]), as[string](t, calls[0]["id"])) {
		t.Fatalf("late result prompt: %q", promptText(prompts[len(prompts)-1]))
	}
}

// TestNewInputInsteadOfResults cancels the waiting turn.
func TestNewInputInsteadOfResults(t *testing.T) {
	e := newEnv(t, nil)
	results := make(chan acptest.ToolResult, 1)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if strings.Contains(t.Text(), "never mind") {
			return sdk.StopReasonEndTurn, t.Say("ok, dropped")
		}
		r, err := t.Call("get_weather", map[string]any{"city": "Paris"})
		// The cancel may reach the MCP client before the error result does.
		if err != nil {
			r = acptest.ToolResult{Text: err.Error(), IsError: true}
		}
		results <- r
		<-t.Ctx.Done()
		return sdk.StopReasonCancelled, nil
	})
	msgs := []any{user("weather")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	if len(toolCalls(t, r1)) != 1 {
		t.Fatalf("calls: %v", r1)
	}
	msgs = append(msgs, message(t, r1), user("never mind"))
	status, r2 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 || message(t, r2)["content"] != "ok, dropped" {
		t.Fatalf("%d %v", status, r2)
	}
	select {
	case r := <-results:
		if !r.IsError {
			t.Fatalf("the abandoned call must get an error result: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned call never settled")
	}
}

// TestClientDisconnectMidStream closes the connection during a turn; the
// gateway cancels the turn and drops the session.
func TestClientDisconnectMidStream(t *testing.T) {
	e := newEnv(t, nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if err := t.Say("partial"); err != nil {
			return "", err
		}
		close(started)
		<-t.Ctx.Done()
		close(canceled)
		return sdk.StopReasonCancelled, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	b, _ := json.Marshal(map[string]any{"model": "mock-1", "stream": true, "messages": []any{user("long")}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		closeBody(resp)
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the runtime turn was not canceled")
	}
	eventually(t, 5*time.Second, "session closed", func() bool { return e.app.Sessions.Len() == 0 && len(e.agent.Closed()) >= 2 })
}

func TestRequestTimeout(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RequestTimeout = 200 * time.Millisecond })
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		<-t.Ctx.Done()
		return sdk.StopReasonCancelled, nil
	})
	status, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("slow")}})
	if status != http.StatusGatewayTimeout {
		t.Fatalf("%d %v", status, resp)
	}
	s := e.stream(map[string]any{"model": "mock-1", "messages": []any{user("slow")}})
	if s.status != http.StatusGatewayTimeout {
		t.Fatalf("stream with no output yet must fail with a status: %+v", s)
	}
}

func TestRuntimeErrors(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if strings.Contains(t.Text(), "quota") {
			return "", sdk.NewInternalError(map[string]any{"error": "RESOURCE_EXHAUSTED: quota"})
		}
		if strings.Contains(t.Text(), "midway") {
			if err := t.Say("half"); err != nil {
				return "", err
			}
		}
		return "", sdk.NewInternalError(map[string]any{"error": "boom"})
	})
	status, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("quota")}})
	if status != http.StatusTooManyRequests {
		t.Fatalf("quota: %d %v", status, resp)
	}
	status, resp = e.chat(map[string]any{"model": "mock-1", "messages": []any{user("x")}})
	if status != http.StatusBadGateway {
		t.Fatalf("error: %d %v", status, resp)
	}
	s := e.stream(map[string]any{"model": "mock-1", "messages": []any{user("midway")}})
	if s.status != 200 || s.done || len(s.errors) != 1 {
		t.Fatalf("an error after output must end the stream with an error event and no [DONE]: %+v", s)
	}
	eventually(t, 5*time.Second, "failed sessions closed", func() bool { return e.app.Sessions.Len() == 0 })
}

func TestUsageNeverInvented(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		t.NoUsage()
		return sdk.StopReasonEndTurn, t.Say("hi")
	})
	_, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("x")}})
	if resp["usage"] != nil {
		t.Fatalf("usage without runtime counts: %v", resp["usage"])
	}
	s := e.stream(map[string]any{"model": "mock-1", "stream_options": map[string]any{"include_usage": true}, "messages": []any{user("x")}})
	if _, _, _, usage := s.assemble(t); usage != nil || !s.done {
		t.Fatalf("stream usage without runtime counts: %v", usage)
	}
}

func TestStopReasons(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if err := t.Say("cut"); err != nil {
			return "", err
		}
		switch t.Text() {
		case "long":
			return sdk.StopReasonMaxTokens, nil
		case "refuse":
			return sdk.StopReasonRefusal, nil
		}
		return sdk.StopReasonEndTurn, nil
	})
	for in, want := range map[string]string{"long": "length", "refuse": "content_filter", "x": "stop"} {
		_, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user(in)}})
		if finish(t, resp) != want {
			t.Fatalf("%s: finish %q, want %q", in, finish(t, resp), want)
		}
	}
}

// TestJSONObject covers response_format json_object: the instruction is
// added to the prompt, a code fence is removed, an invalid reply is asked
// for once more, and a second invalid reply fails the request.
func TestJSONObject(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		first := promptText(t.Session.Prompts[0])
		switch {
		case !strings.Contains(first, "JSON object"):
			return sdk.StopReasonEndTurn, t.Say("the instruction is missing")
		case strings.Contains(first, "fenced"):
			return sdk.StopReasonEndTurn, t.Say("```json\n{\"a\": 1}\n```")
		case strings.Contains(first, "never"):
			return sdk.StopReasonEndTurn, t.Say("no JSON here")
		case strings.Contains(t.Text(), "not a single valid JSON object"):
			return sdk.StopReasonEndTurn, t.Say(`{"b": 2}`)
		default:
			return sdk.StopReasonEndTurn, t.Say("[1, 2]")
		}
	})
	jsonReq := func(text string) map[string]any {
		return map[string]any{"model": "mock-1", "response_format": map[string]any{"type": "json_object"}, "messages": []any{user(text)}}
	}

	status, resp := e.chat(jsonReq("fenced"))
	if status != 200 || message(t, resp)["content"] != `{"a": 1}` {
		t.Fatalf("fenced: %d %v", status, resp)
	}
	s := e.stream(jsonReq("fenced"))
	if content, _, finish, _ := s.assemble(t); content != `{"a": 1}` || finish != "stop" || !s.done {
		t.Fatalf("fenced stream: %q %q %+v", content, finish, s)
	}

	status, resp = e.chat(jsonReq("array"))
	if status != 200 || message(t, resp)["content"] != `{"b": 2}` {
		t.Fatalf("retry: %d %v", status, resp)
	}
	usage := as[map[string]any](t, resp["usage"])
	if usage["prompt_tokens"] != float64(200) {
		t.Fatalf("usage must cover both turns: %v", usage)
	}

	status, resp = e.chat(jsonReq("never"))
	if status != http.StatusBadGateway {
		t.Fatalf("never: %d %v", status, resp)
	}
	s = e.stream(jsonReq("never"))
	if s.status != http.StatusBadGateway || len(s.chunks) != 0 {
		t.Fatalf("an invalid reply must not reach a streaming client: %+v", s)
	}
}

func TestForeignToolsAreRejected(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		ok, err := t.RequestForeignPermission("run_shell_command")
		if err != nil {
			return "", err
		}
		return sdk.StopReasonEndTurn, t.Say(fmt.Sprint(ok))
	})
	_, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("x")}})
	if message(t, resp)["content"] != "false" {
		t.Fatalf("runtime tool was allowed: %v", resp)
	}
}

// TestOwnersAreIsolated: another key, or another user field, never lands
// in someone else's runtime session.
func TestOwnersAreIsolated(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(weatherAgent)
	msgs := []any{user("weather in Paris")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	calls := toolCalls(t, r1)
	if len(calls) != 1 {
		t.Fatalf("%v", r1)
	}
	msgs = append(msgs, message(t, r1), toolResult(as[string](t, calls[0]["id"]), "spoofed"))
	status, r2 := e.chatAs("other-key", map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 {
		t.Fatalf("%d %v", status, r2)
	}
	if strings.Contains(fmt.Sprint(r2), "Weather: Paris=spoofed") {
		t.Fatal("another key resolved the call")
	}
	if e.app.Sessions.Len() != 2 {
		t.Fatalf("want a separate session, have %d", e.app.Sessions.Len())
	}
	status, r3 := e.chat(map[string]any{"model": "mock-1", "user": "alice", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 || strings.Contains(fmt.Sprint(r3), "Weather: Paris=spoofed") || e.app.Sessions.Len() != 3 {
		t.Fatalf("user field must isolate: %d %v (sessions %d)", status, r3, e.app.Sessions.Len())
	}
}

func TestConcurrentResultsAreBusy(t *testing.T) {
	e := newEnv(t, nil)
	release := make(chan struct{})
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if _, err := t.Call("get_weather", map[string]any{"city": "Paris"}); err != nil {
			return "", err
		}
		<-release
		return sdk.StopReasonEndTurn, t.Say("done")
	})
	msgs := []any{user("w")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	calls := toolCalls(t, r1)
	msgs = append(msgs, message(t, r1), toolResult(as[string](t, calls[0]["id"]), "1"))
	body := map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs}
	first := make(chan int, 1)
	go func() {
		s, _ := e.chat(body)
		first <- s
	}()
	eventually(t, 5*time.Second, "result delivered", func() bool { return e.agent.ToolCalls() == 1 && len(first) == 0 })
	time.Sleep(50 * time.Millisecond)
	status, resp := e.chat(body)
	if status != http.StatusConflict {
		t.Fatalf("want 409 while the first request is in flight: %d %v", status, resp)
	}
	close(release)
	if s := <-first; s != 200 {
		t.Fatalf("first: %d", s)
	}
}

func TestEvictionWhenFull(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.MaxSessions = 1 })
	_, _ = e.chat(map[string]any{"model": "mock-1", "messages": []any{user("one")}})
	first := e.agent.Sessions()
	_, _ = e.chat(map[string]any{"model": "mock-1", "messages": []any{user("two")}})
	eventually(t, 5*time.Second, "eviction", func() bool { return slices.Contains(e.agent.Closed(), first[0]) })
	if e.app.Sessions.Len() != 1 {
		t.Fatalf("sessions: %d", e.app.Sessions.Len())
	}

	// A full gateway with only busy sessions answers 429.
	release := make(chan struct{})
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		<-release
		return sdk.StopReasonEndTurn, t.Say("ok")
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.chat(map[string]any{"model": "mock-1", "messages": []any{user("three")}})
	}()
	eventually(t, 5*time.Second, "busy session", func() bool { return len(e.agent.Sessions()) == 1 && e.agent.Sessions()[0] != first[0] })
	time.Sleep(50 * time.Millisecond)
	status, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("four")}})
	close(release)
	<-done
	if status != http.StatusTooManyRequests {
		t.Fatalf("want 429: %d %v", status, resp)
	}
}

// TestCrashResumesIdleSessions restarts the runtime between turns.
func TestCrashResumesIdleSessions(t *testing.T) {
	e := newEnv(t, nil)
	msgs := []any{user("hello")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "messages": msgs})
	sid := e.agent.Sessions()[0]
	e.agent.Crash()
	eventually(t, 10*time.Second, "restart", func() bool { return e.agent.Starts() == 2 })
	e.waitReady()
	msgs = append(msgs, message(t, r1), user("again"))
	status, r2 := e.chat(map[string]any{"model": "mock-1", "messages": msgs})
	if status != 200 || message(t, r2)["content"] != "ok" {
		t.Fatalf("%d %v", status, r2)
	}
	if !slices.Contains(e.agent.Resumed(), sid) || len(e.agent.PromptsOf(sid)) != 2 {
		t.Fatalf("session was not resumed: resumed %v", e.agent.Resumed())
	}
}

// TestCrashMidTurnReplays loses a session while a tool call is pending;
// the client's result then starts a new session with a replayed transcript.
func TestCrashMidTurnReplays(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if strings.Contains(t.Text(), "<conversation>") {
			return sdk.StopReasonEndTurn, t.Say("recovered")
		}
		_, err := t.Call("get_weather", map[string]any{"city": "Paris"})
		return sdk.StopReasonEndTurn, err
	})
	msgs := []any{user("weather in Paris")}
	_, r1 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	calls := toolCalls(t, r1)
	if len(calls) != 1 {
		t.Fatalf("%v", r1)
	}
	e.agent.Crash()
	eventually(t, 10*time.Second, "restart", func() bool { return e.agent.Starts() == 2 })
	e.waitReady()
	eventually(t, 5*time.Second, "lost session closed", func() bool { return e.app.Sessions.Len() == 0 })
	msgs = append(msgs, message(t, r1), toolResult(as[string](t, calls[0]["id"]), "rain"))
	status, r2 := e.chat(map[string]any{"model": "mock-1", "tools": []any{weatherTool}, "messages": msgs})
	if status != 200 || message(t, r2)["content"] != "recovered" {
		t.Fatalf("%d %v", status, r2)
	}
	ids := e.agent.Sessions()
	p := e.agent.PromptsOf(ids[len(ids)-1])
	txt := promptText(p[0])
	if !strings.Contains(txt, "weather in Paris") || !strings.Contains(txt, "rain") {
		t.Fatalf("replay prompt: %q", txt)
	}
}

func TestNotReadyIs503(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.Crash()
	eventually(t, 5*time.Second, "down", func() bool { ok, _ := e.app.Runtime.Ready(); return !ok })
	status, resp := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("x")}})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("%d %v", status, resp)
	}
}

const usageReport = "### Session Usage\n- **Session total:** data not available yet\n\n### Quota Limits\n- **Gemini Models**\n  - **Weekly Limit Remaining:** 40% remaining (resets Oct 02, 2026, 08:22 UTC)\n"

// TestQuotaExhausted covers the runtime's quota message: it becomes a 429
// that says when to retry, later requests fail fast until then, and a
// quota report with limits left lifts the block.
func TestQuotaExhausted(t *testing.T) {
	e := newEnv(t, nil)
	var prompts atomic.Int32
	e.agent.OnPrompt(func(t *acptest.Turn) (sdk.StopReason, error) {
		if t.Text() == "/usage" {
			return sdk.StopReasonEndTurn, t.Say(usageReport)
		}
		prompts.Add(1)
		return sdk.StopReasonEndTurn, t.Say("Usage Limit Reached\n\nYou have reached your current quota for this period. Your limit will reset in 2 hours, 30 minutes.")
	})

	body := map[string]any{"model": "mock-1", "stream": true, "messages": []any{user("x")}}
	resp := e.do(http.MethodPost, "/v1/chat/completions", key, body)
	var reply struct {
		Error struct {
			Type            string `json:"type"`
			ResetsAt        int64  `json:"resets_at"`
			ResetsInSeconds int64  `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	closeBody(resp)
	ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	if resp.StatusCode != http.StatusTooManyRequests || reply.Error.Type != "usage_limit_reached" ||
		ra < 9000-5 || ra > 9000 || reply.Error.ResetsInSeconds != int64(ra) || reply.Error.ResetsAt < time.Now().Unix()+8990 {
		t.Fatalf("quota: %d Retry-After %q %+v", resp.StatusCode, resp.Header.Get("Retry-After"), reply)
	}

	status, out := e.chat(map[string]any{"model": "mock-1", "messages": []any{user("y")}})
	if status != http.StatusTooManyRequests || prompts.Load() != 1 {
		t.Fatalf("a request before the reset must fail without reaching the runtime: %d %v (prompts %d)", status, out, prompts.Load())
	}

	resp = e.do(http.MethodGet, "/v1/quota", key, nil)
	var q struct {
		Limited bool `json:"limited"`
		Groups  []struct {
			Name    string `json:"name"`
			Buckets []struct {
				Name             string `json:"name"`
				RemainingPercent int    `json:"remaining_percent"`
				ResetsAt         int64  `json:"resets_at"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	closeBody(resp)
	if resp.StatusCode != http.StatusOK || q.Limited || len(q.Groups) != 1 || q.Groups[0].Name != "Gemini Models" ||
		len(q.Groups[0].Buckets) != 1 || q.Groups[0].Buckets[0].RemainingPercent != 40 ||
		q.Groups[0].Buckets[0].ResetsAt != time.Date(2026, 10, 2, 8, 22, 0, 0, time.UTC).Unix() {
		t.Fatalf("quota report: %d %+v", resp.StatusCode, q)
	}
	e.chat(map[string]any{"model": "mock-1", "messages": []any{user("z")}})
	if prompts.Load() != 2 {
		t.Fatalf("a report with limits left must lift the block (prompts %d)", prompts.Load())
	}
	eventually(t, 5*time.Second, "sessions closed", func() bool { return e.app.Sessions.Len() == 0 })
}
