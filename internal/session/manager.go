// Package session maps stateless OpenAI chat requests onto long-lived ACP
// runtime sessions. A conversation is found again either by the tool call
// ids the gateway issued or by the longest recorded message prefix, so no
// custom headers are needed and earlier history is not re-sent.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/acp"
	"github.com/sujoshua/agy-gateway/internal/bridge"
	"github.com/sujoshua/agy-gateway/internal/openai"
)

// Options configures a Manager.
type Options struct {
	Runtime *acp.Runtime
	Bridge  *bridge.Bridge
	// BridgeURL returns the base URL of the bridge listener, e.g.
	// http://127.0.0.1:41234.
	BridgeURL     func() string
	MCPServerName string
	TTL           time.Duration
	MaxSessions   int
	BatchWindow   time.Duration
	ReplayBudget  int
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// Manager owns all sessions of one runtime.
type Manager struct {
	opt Options
	log *slog.Logger
	ctx context.Context // lifetime of the gateway; turns run on it

	mu       sync.Mutex
	sessions map[string]*Session // by ACP session id
	calls    map[string]*Session // by issued tool call id
	limited  time.Time           // requests get a 429 until then

	quotaMu sync.Mutex
	quota   *Quota // last report, guarded by quotaMu
	quotaAt time.Time
}

// NewManager creates a Manager. Route runtime updates to Dispatch and
// runtime restarts to RuntimeRestarted.
func NewManager(ctx context.Context, opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.SweepInterval <= 0 {
		opt.SweepInterval = time.Minute
	}
	return &Manager{opt: opt, log: opt.Logger, ctx: ctx, sessions: map[string]*Session{}, calls: map[string]*Session{}}
}

// Request is a validated chat request.
type Request struct {
	Owner      string
	Model      acp.Model
	Messages   []openai.Message
	Tools      []openai.Tool // empty when tool_choice is "none"
	EmitThinks bool
	// SingleToolCall answers with at most one tool call
	// (parallel_tool_calls: false); later calls wait for the next request.
	SingleToolCall bool
	// JSONObject asks for a final answer that is one JSON object
	// (response_format json_object).
	JSONObject bool
}

// Emitter receives response output as it is produced.
type Emitter interface {
	Text(s string) error
	Reasoning(s string) error
	// ToolCall emits the index-th tool call of this response.
	ToolCall(index int, c *bridge.Call) error
}

// Outcome is the end of one response.
type Outcome struct {
	Content      string
	Reasoning    string
	ToolCalls    []*bridge.Call
	FinishReason string
	// Usage is set only on the response that ends a runtime turn. It covers
	// the whole turn, including model calls made between tool results.
	Usage *openai.Usage
}

// Partial reports whether output reached the client before an error.
type Partial struct {
	Err  *openai.Error
	Sent bool
	// keep is set for errors found before anything reached the runtime;
	// the session stays usable.
	keep bool
}

// Len returns the number of live sessions.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Dispatch routes a session/update notification to its session.
func (m *Manager) Dispatch(n sdk.SessionNotification) {
	m.mu.Lock()
	s := m.sessions[string(n.SessionId)]
	m.mu.Unlock()
	if s != nil {
		s.onUpdate(n)
	}
}

// RuntimeRestarted handles a runtime crash: sessions mid-turn are lost and
// closed; idle sessions are resumed from the runtime's stored state on
// their next request.
func (m *Manager) RuntimeRestarted(error) {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	var lost []*Session
	for _, s := range all {
		// The crash may have ended the turn before this runs; calls still
		// waiting for results mean it was cut short all the same.
		s.mu.Lock()
		waiting := len(s.emitted) > 0
		s.mu.Unlock()
		m.mu.Lock()
		if s.busy || waiting || s.currentTurn().active() {
			lost = append(lost, s)
		} else {
			s.needsResume = true
		}
		m.mu.Unlock()
	}
	for _, s := range lost {
		m.close(s, "runtime restarted mid-turn")
	}
}

// Run sweeps idle sessions until ctx ends, then closes all sessions.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.opt.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.CloseAll()
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func (m *Manager) sweep() {
	cutoff := time.Now().Add(-m.opt.TTL)
	m.mu.Lock()
	var old []*Session
	for _, s := range m.sessions {
		if !s.busy && s.lastUsed.Before(cutoff) {
			old = append(old, s)
		}
	}
	m.mu.Unlock()
	for _, s := range old {
		m.close(s, "idle")
	}
}

// CloseAll closes every session.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		m.close(s, "shutdown")
	}
}

// close removes a session, cancels its turn and deletes its runtime state.
func (m *Manager) close(s *Session, reason string) {
	m.mu.Lock()
	if s.closed {
		m.mu.Unlock()
		return
	}
	s.closed = true
	delete(m.sessions, s.id)
	for id, owner := range m.calls {
		if owner == s {
			delete(m.calls, id)
		}
	}
	m.mu.Unlock()
	m.log.Info("closing session", logSession(s.id), logReason(reason))
	m.opt.Bridge.Remove(s.tenant)
	t := s.currentTurn()
	ctx := context.WithoutCancel(m.ctx)
	if t.active() {
		m.opt.Runtime.Cancel(ctx, s.id)
	}
	go func() {
		if t != nil {
			select {
			case <-t.done:
			case <-time.After(30 * time.Second):
			}
		}
		m.opt.Runtime.CloseSession(ctx, s.id)
		m.opt.Runtime.RemoveWorkDir(s.workDir)
	}()
}

// Handle serves one chat request. On error, Partial.Sent tells whether the
// client already received output.
func (m *Manager) Handle(ctx context.Context, rq *Request, em Emitter) (*Outcome, *Partial) {
	if _, err := m.opt.Runtime.Conn(); err != nil {
		return nil, &Partial{Err: openai.Unavailable("%v", err)}
	}
	if at := m.Limited(); !at.IsZero() {
		return nil, &Partial{Err: openai.UsageLimit(at, time.Now())}
	}
	fps := fingerprints(rq.Messages)
	s, start, oerr := m.acquire(rq, fps)
	if oerr != nil {
		return nil, &Partial{Err: oerr}
	}
	if s != nil {
		if err := m.prepare(ctx, s, rq); err != nil {
			m.log.Warn("cannot reuse session; starting a new one", logSession(s.id), slog.Any("err", err))
			m.release(s, nil)
			m.close(s, "reuse failed")
			s = nil
		}
	}
	fresh := s == nil
	if fresh {
		var err *openai.Error
		if s, err = m.create(ctx, rq); err != nil {
			return nil, &Partial{Err: err}
		}
		start = trailingStart(rq.Messages)
	}

	var j *jsonEmitter
	if rq.JSONObject {
		j = &jsonEmitter{next: em}
		em = j
	}
	out, p := m.serve(ctx, s, rq, start, fresh, em)
	if j != nil {
		if p == nil {
			out, p = m.finishJSON(ctx, s, rq, j, out)
		} else {
			p.Sent = j.sent
		}
	}
	if p != nil {
		m.release(s, nil)
		if !p.keep {
			m.close(s, "request failed: "+p.Err.Message)
		}
		return nil, p
	}
	m.release(s, append(fps, fingerprint(assistantMessage(out))))
	return out, nil
}

// prepare resumes a session after a runtime restart and applies the model.
func (m *Manager) prepare(ctx context.Context, s *Session, rq *Request) error {
	m.mu.Lock()
	resume := s.needsResume
	m.mu.Unlock()
	if resume {
		conn, err := m.opt.Runtime.Conn()
		if err != nil {
			return err
		}
		if _, err := conn.ResumeSession(ctx, sdk.ResumeSessionRequest{
			SessionId: sdk.SessionId(s.id), Cwd: s.workDir, McpServers: m.mcpServers(s.tenant),
		}); err != nil {
			return fmt.Errorf("session/resume: %w", err)
		}
		m.mu.Lock()
		s.needsResume = false
		s.model = acp.Model{}
		m.mu.Unlock()
	}
	if s.model != rq.Model && !s.currentTurn().active() {
		if err := m.opt.Runtime.ApplyModel(ctx, s.id, rq.Model); err != nil {
			return err
		}
		s.model = rq.Model
	}
	return nil
}

// acquire finds the session for a request and marks it busy. It returns
// nil when a new session is needed; start is the first message the runtime
// has not seen.
func (m *Manager) acquire(rq *Request, fps []string) (s *Session, start int, oerr *openai.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Tool results for calls this gateway issued.
	lastAssistant := -1
	for i, msg := range rq.Messages {
		if msg.Role == "assistant" {
			lastAssistant = i
		}
	}
	for _, msg := range rq.Messages[lastAssistant+1:] {
		if msg.Role != "tool" {
			continue
		}
		owner := m.calls[msg.ToolCallID]
		if owner == nil || owner.owner != rq.Owner {
			continue
		}
		if s != nil && s != owner {
			return nil, 0, openai.InvalidRequest("messages", "tool results belong to different conversations")
		}
		s = owner
	}
	if s != nil {
		if s.busy {
			return nil, 0, openai.Busy("this conversation is already being served by another request")
		}
		s.busy = true
		return s, lastAssistant + 1, nil
	}

	// 2. Longest recorded prefix: the client re-sent what the runtime saw
	// and appended new user input.
	key := toolsKey(rq.Tools)
	for _, c := range m.sessions {
		if c.owner != rq.Owner || c.toolsKey != key || c.busy || len(c.history) >= len(fps) {
			continue
		}
		if s != nil && len(c.history) <= len(s.history) {
			continue
		}
		if !slices.Equal(c.history, fps[:len(c.history)]) {
			continue
		}
		onlyInput := true
		for _, msg := range rq.Messages[len(c.history):] {
			if msg.Role != "user" && msg.Role != "tool" {
				onlyInput = false
			}
		}
		if onlyInput {
			s = c
		}
	}
	if s != nil {
		s.busy = true
		return s, len(s.history), nil
	}
	return nil, 0, nil
}

// release ends the request's hold on a session and records its history.
func (m *Manager) release(s *Session, history []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.busy = false
	s.lastUsed = time.Now()
	if history != nil {
		s.history = history
	}
}

func (m *Manager) mcpServers(t *bridge.Tenant) []sdk.McpServer {
	if t == nil {
		return []sdk.McpServer{}
	}
	return []sdk.McpServer{{Http: &sdk.McpServerHttpInline{
		Type:    "http",
		Name:    m.opt.MCPServerName,
		Url:     m.opt.BridgeURL() + "/mcp/" + t.ID,
		Headers: []sdk.HttpHeader{{Name: "Authorization", Value: "Bearer " + t.Secret}},
	}}}
}

// create starts a runtime session for the request and marks it busy.
func (m *Manager) create(ctx context.Context, rq *Request) (*Session, *openai.Error) {
	if err := m.makeRoom(); err != nil {
		return nil, err
	}
	conn, err := m.opt.Runtime.Conn()
	if err != nil {
		return nil, openai.Unavailable("%v", err)
	}
	dir, err := m.opt.Runtime.NewWorkDir()
	if err != nil {
		return nil, openai.Internal("cannot create a session directory")
	}
	s := &Session{
		m: m, owner: rq.Owner, toolsKey: toolsKey(rq.Tools), workDir: dir, q: newQueue(),
		busy: true, lastUsed: time.Now(),
		emitted: map[string]*bridge.Call{}, answered: map[string]bool{}, expired: map[string]*bridge.Call{},
	}
	if len(rq.Tools) > 0 {
		s.tenant = m.opt.Bridge.Register(rq.Tools, s)
	}
	req := sdk.NewSessionRequest{Cwd: dir, McpServers: m.mcpServers(s.tenant)}
	if sys := systemText(rq.Messages); sys != "" {
		req.Meta = map[string]any{"antigravity": map[string]any{"systemPrompt": map[string]any{"append": sys}}}
	}
	resp, err := conn.NewSession(ctx, req)
	if err != nil {
		m.opt.Bridge.Remove(s.tenant)
		m.opt.Runtime.RemoveWorkDir(dir)
		return nil, upstreamError(err)
	}
	s.id = string(resp.SessionId)
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	if err := m.opt.Runtime.ApplyModel(ctx, s.id, rq.Model); err != nil {
		m.close(s, "model selection failed")
		return nil, upstreamError(err)
	}
	s.model = rq.Model
	m.log.Info("session created", logSession(s.id), slog.Int("tools", len(rq.Tools)), slog.String("model", rq.Model.ID))
	return s, nil
}

// makeRoom evicts the least recently used idle session when full.
func (m *Manager) makeRoom() *openai.Error {
	m.mu.Lock()
	if m.opt.MaxSessions <= 0 || len(m.sessions) < m.opt.MaxSessions {
		m.mu.Unlock()
		return nil
	}
	var victim *Session
	for _, s := range m.sessions {
		if !s.busy && (victim == nil || s.lastUsed.Before(victim.lastUsed)) {
			victim = s
		}
	}
	m.mu.Unlock()
	if victim == nil {
		return openai.TooManySessions()
	}
	m.close(victim, "evicted (max sessions)")
	return nil
}

// serve delivers the new input to the runtime and streams the response.
func (m *Manager) serve(ctx context.Context, s *Session, rq *Request, start int, fresh bool, em Emitter) (*Outcome, *Partial) {
	names := callNames(rq.Messages)
	input := rq.Messages[start:]
	if t := s.currentTurn(); t.active() {
		pending, err := s.resolve(input)
		if err != nil {
			return nil, &Partial{Err: err, keep: true}
		}
		if len(pending) == 0 {
			return m.consume(ctx, s, rq, em)
		}
		// Input other than results for the waiting calls cannot be added to
		// a running turn: cancel it and send everything as a new prompt.
		s.abandon("the client sent new input instead of tool results")
		m.opt.Runtime.Cancel(ctx, s.id)
		select {
		case <-t.done:
		case <-ctx.Done():
			return nil, &Partial{Err: openai.Timeout("timed out canceling the previous turn")}
		}
	} else if err := s.checkLate(input); err != nil {
		return nil, &Partial{Err: err, keep: true}
	}

	var blocks []sdk.ContentBlock
	if fresh && start > 0 {
		prior := rq.Messages[:start]
		if text := transcript(prior, m.opt.ReplayBudget); text != "" {
			blocks = append(blocks, sdk.TextBlock("The conversation so far, replayed by the gateway because the previous runtime session is gone:\n<conversation>\n"+
				text+"\n</conversation>\nContinue the conversation from here. New input follows."))
		}
	}
	blocks = append(blocks, promptBlocks(input, names)...)
	if len(blocks) == 0 {
		return nil, &Partial{Err: openai.InvalidRequest("messages", "the request contains no new input for the model"), keep: true}
	}
	if rq.JSONObject {
		blocks = append(blocks, sdk.TextBlock(jsonInstruction))
	}
	s.markAnswered(input)
	s.startTurn(m.ctx, blocks)
	return m.consume(ctx, s, rq, em)
}

// consume relays turn events until the turn ends or tool calls are ready.
func (m *Manager) consume(ctx context.Context, s *Session, rq *Request, em Emitter) (*Outcome, *Partial) {
	out := &Outcome{}
	sent := false
	quota := ""
	fail := func(err *openai.Error) (*Outcome, *Partial) { return nil, &Partial{Err: err, Sent: sent} }
	var batch <-chan time.Time
	for {
		ev, ok := s.q.pop()
		if !ok {
			select {
			case <-s.q.wake:
				continue
			case <-batch:
				out.FinishReason = "tool_calls"
				return out, nil
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return fail(openai.Timeout("the request exceeded the gateway timeout"))
				}
				return fail(openai.Internal("the client went away"))
			}
		}
		var err error
		switch ev.kind {
		case evText:
			out.Content += ev.text
			sent = true
			err = em.Text(ev.text)
		case evThought:
			if rq.EmitThinks {
				out.Reasoning += ev.text
				sent = true
				err = em.Reasoning(ev.text)
			}
		case evToolCall:
			if ev.call.Settled() {
				continue
			}
			s.emit(ev.call)
			m.mu.Lock()
			m.calls[ev.call.ID] = s
			m.mu.Unlock()
			out.ToolCalls = append(out.ToolCalls, ev.call)
			sent = true
			err = em.ToolCall(len(out.ToolCalls)-1, ev.call)
			if err == nil && rq.SingleToolCall {
				out.FinishReason = "tool_calls"
				return out, nil
			}
			if batch == nil {
				batch = time.After(m.opt.BatchWindow)
			}
		case evQuota:
			quota = ev.text
		case evDone:
			if ev.err != nil {
				return fail(upstreamError(ev.err))
			}
			if quota != "" {
				now := time.Now()
				at := quotaReset(quota, now)
				m.limitUntil(at)
				m.log.Warn("the account quota is exhausted", logSession(s.id), slog.Time("resets_at", at))
				return fail(openai.UsageLimit(at, now))
			}
			out.FinishReason = finishReason(ev.stop)
			if len(out.ToolCalls) > 0 {
				// The turn ended while calls were being batched (the bridge
				// settled them). The client already saw the calls; their
				// results will start a new turn as late results.
				out.FinishReason = "tool_calls"
			}
			out.Usage = usage(ev.usage)
			return out, nil
		}
		if err != nil {
			return fail(openai.Internal("writing the response failed"))
		}
	}
}

func (s *Session) startTurn(ctx context.Context, blocks []sdk.ContentBlock) {
	t := &turn{done: make(chan struct{})}
	s.q.reset()
	s.mu.Lock()
	s.cur = t
	s.mu.Unlock()
	conn, err := s.m.opt.Runtime.Conn()
	go func() {
		defer close(t.done)
		if err != nil {
			s.q.push(event{kind: evDone, err: err})
			return
		}
		resp, err := conn.Prompt(ctx, sdk.PromptRequest{SessionId: sdk.SessionId(s.id), Prompt: blocks})
		s.q.push(event{kind: evDone, stop: resp.StopReason, usage: resp.Usage, err: err})
	}()
}

func (s *Session) emit(c *bridge.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitted[c.ID] = c
}

// resolve hands tool results to the calls waiting in the bridge. It returns
// the input that is not a result for a waiting call.
func (s *Session) resolve(input []openai.Message) ([]openai.Message, *openai.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rest []openai.Message
	for _, msg := range input {
		if msg.Role == "tool" && s.answered[msg.ToolCallID] {
			return nil, openai.InvalidRequest("messages", "the result for tool call %q was already submitted", msg.ToolCallID)
		}
		if msg.Role == "tool" && s.emitted[msg.ToolCallID] != nil {
			continue
		}
		rest = append(rest, msg)
	}
	if len(rest) > 0 {
		return rest, nil
	}
	for _, msg := range input {
		c := s.emitted[msg.ToolCallID]
		delete(s.emitted, c.ID)
		s.answered[c.ID] = true
		c.Resolve(bridge.Result{Content: msg.Content.Text()})
	}
	// OpenAI clients answer every call of a response; any call left without
	// a result is reported to the model as an error so the turn can go on.
	for id, c := range s.emitted {
		delete(s.emitted, id)
		s.answered[id] = true
		c.Resolve(bridge.Result{Content: "The client returned no result for this tool call.", IsError: true})
	}
	return nil, nil
}

// abandon settles waiting calls when their turn is being canceled.
func (s *Session) abandon(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.emitted {
		delete(s.emitted, id)
		s.expired[id] = c
		c.Resolve(bridge.Result{Content: reason, IsError: true})
	}
}

// checkLate rejects results already consumed. Results for calls whose turn
// ended are sent as ordinary input of a new turn.
func (s *Session) checkLate(input []openai.Message) *openai.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range input {
		if msg.Role == "tool" && s.answered[msg.ToolCallID] {
			return openai.InvalidRequest("messages", "the result for tool call %q was already submitted", msg.ToolCallID)
		}
	}
	return nil
}

func (s *Session) markAnswered(input []openai.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range input {
		if msg.Role == "tool" {
			s.answered[msg.ToolCallID] = true
			delete(s.emitted, msg.ToolCallID)
			delete(s.expired, msg.ToolCallID)
		}
	}
}

func assistantMessage(out *Outcome) openai.Message {
	msg := openai.Message{Role: "assistant"}
	if out.Content != "" {
		msg.Content = openai.TextContent(out.Content)
	}
	for _, c := range out.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{ID: c.ID, Type: "function", Function: openai.FunctionCall{Name: c.Name, Arguments: c.Args}})
	}
	return msg
}

func finishReason(stop sdk.StopReason) string {
	switch stop {
	case sdk.StopReasonMaxTokens, sdk.StopReasonMaxTurnRequests:
		return "length"
	case sdk.StopReasonRefusal:
		return "content_filter"
	default:
		return "stop"
	}
}

// usage converts runtime token counts. It returns nil when the runtime
// reported none; counts are never estimated.
func usage(u *sdk.Usage) *openai.Usage {
	if u == nil {
		return nil
	}
	thought := 0
	if u.ThoughtTokens != nil {
		thought = *u.ThoughtTokens
	}
	cached := 0
	if u.CachedReadTokens != nil {
		cached = *u.CachedReadTokens
	}
	return &openai.Usage{
		PromptTokens:            u.InputTokens,
		CompletionTokens:        u.OutputTokens + thought,
		TotalTokens:             u.TotalTokens,
		PromptTokensDetails:     &openai.PromptTokensDetails{CachedTokens: cached},
		CompletionTokensDetails: &openai.CompletionTokensDetails{ReasoningTokens: thought},
	}
}

func upstreamError(err error) *openai.Error {
	if errors.Is(err, acp.ErrNotReady) {
		return openai.Unavailable("%v", err)
	}
	msg := err.Error()
	if re, ok := errors.AsType[*sdk.RequestError](err); ok {
		msg = re.Message
		// The detail (for example RESOURCE_EXHAUSTED) is often only in data.
		if re.Data != nil {
			if d, jerr := json.Marshal(re.Data); jerr == nil {
				msg += ": " + string(d)
			}
		}
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "resource_exhausted") || strings.Contains(low, "quota") || strings.Contains(low, "rate limit") || strings.Contains(low, "429") {
		return openai.RateLimited("the runtime reported a rate limit: %s", msg)
	}
	if strings.Contains(low, "connection closed") || strings.Contains(low, "peer disconnected") {
		return openai.Unavailable("the runtime connection was lost")
	}
	return openai.Upstream("the runtime turn failed: %s", msg)
}

func logSession(id string) slog.Attr { return slog.String("session", id) }
func logCall(id string) slog.Attr    { return slog.String("call_id", id) }
func logReason(r string) slog.Attr   { return slog.String("reason", r) }
func logTitle(t string) slog.Attr    { return slog.String("title", t) }
