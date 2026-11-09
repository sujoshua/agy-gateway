// Package server is the OpenAI-compatible HTTP front end.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sujoshua/agy-gateway/internal/acp"
	"github.com/sujoshua/agy-gateway/internal/openai"
	"github.com/sujoshua/agy-gateway/internal/session"
)

// maxBody bounds a request body; base64 images make requests large.
const maxBody = 32 << 20

// Options configures the server.
type Options struct {
	APIKeys           []string
	RequestTimeout    time.Duration
	IgnoreUnsupported bool
	EmitReasoning     bool
	// Harness is reported by /readyz.
	Harness func() string
	// Keepalive is the idle interval after which a stream sends an SSE
	// comment so proxies keep the connection open.
	Keepalive time.Duration
	Logger    *slog.Logger
}

// Server serves the public API.
type Server struct {
	opt Options
	rt  *acp.Runtime
	mgr *session.Manager
	log *slog.Logger
}

// New creates a Server.
func New(opt Options, rt *acp.Runtime, mgr *session.Manager) *Server {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Keepalive <= 0 {
		opt.Keepalive = 15 * time.Second
	}
	return &Server{opt: opt, rt: rt, mgr: mgr, log: opt.Logger}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		ok, status := s.rt.Ready()
		code := http.StatusOK
		if !ok {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{"ready": ok, "status": status, "sessions": s.mgr.Len(), "harness": s.harness()})
	})
	s.loginRoutes(mux)
	mux.HandleFunc("GET /v1/models", s.auth(s.models))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.chat))
	mux.HandleFunc("GET /v1/quota", s.auth(s.quota))
	mux.HandleFunc("/v1/responses", s.auth(func(w http.ResponseWriter, _ *http.Request, _ string) {
		writeError(w, openai.NotFound("this gateway implements only /v1/chat/completions; configure the client for Chat Completions"))
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, openai.NotFound("unknown route %s %s", r.Method, r.URL.Path))
	})
	return mux
}

func (s *Server) harness() string {
	if s.opt.Harness == nil {
		return ""
	}
	return s.opt.Harness()
}

// auth checks the bearer key and passes an owner id: a hash of the key,
// so conversations are never shared between keys.
func (s *Server) auth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		key = strings.TrimSpace(key)
		if len(s.opt.APIKeys) > 0 {
			ok := 0
			for _, k := range s.opt.APIKeys {
				ok |= subtle.ConstantTimeCompare([]byte(key), []byte(k))
			}
			if ok != 1 {
				writeError(w, openai.Unauthorized())
				return
			}
		}
		sum := sha256.Sum256([]byte(key))
		next(w, r, hex.EncodeToString(sum[:16]))
	}
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request, _ string) {
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	for _, m := range s.rt.Models() {
		list.Data = append(list.Data, openai.Model{ID: m.ID, Object: "model", OwnedBy: "antigravity"})
	}
	if ok, status := s.rt.Ready(); !ok && len(list.Data) == 0 {
		writeError(w, openai.Unavailable("the runtime is not ready: %s", status))
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// quotaReport is the body of GET /v1/quota.
type quotaReport struct {
	Object string `json:"object"`
	// Limited is set while chat requests are refused because the quota is
	// exhausted; ResetsAt (Unix seconds) is when that ends.
	Limited  bool   `json:"limited"`
	ResetsAt *int64 `json:"resets_at,omitempty"`
	*session.Quota
}

func (s *Server) quota(w http.ResponseWriter, r *http.Request, _ string) {
	if ok, status := s.rt.Ready(); !ok {
		writeError(w, openai.Unavailable("the runtime is not ready: %s", status))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	q, err := s.mgr.Quota(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	rep := quotaReport{Object: "quota", Quota: q}
	if at := s.mgr.Limited(); !at.IsZero() {
		unix := at.Unix()
		rep.Limited, rep.ResetsAt = true, &unix
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, owner string) {
	var req openai.ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, openai.InvalidRequest("", "request body is larger than %d bytes", maxBody))
			return
		}
		writeError(w, openai.InvalidRequest("", "invalid JSON body: %v", err))
		return
	}
	choice, verr := openai.Validate(&req, s.opt.IgnoreUnsupported)
	if verr != nil {
		writeError(w, verr)
		return
	}
	if ok, status := s.rt.Ready(); !ok {
		writeError(w, openai.Unavailable("the runtime is not ready: %s", status))
		return
	}
	model, ok := s.rt.FindModel(req.Model, req.ReasoningEffort)
	if !ok {
		writeError(w, openai.ModelNotFound(req.Model))
		return
	}
	tools := req.Tools
	if choice == openai.ToolChoiceNone {
		tools = nil
	}
	if req.User != "" {
		sum := sha256.Sum256([]byte(owner + "\x00" + req.User))
		owner = hex.EncodeToString(sum[:16])
	}
	rq := &session.Request{
		Owner: owner, Model: model, Messages: req.Messages, Tools: tools, EmitThinks: s.opt.EmitReasoning,
		SingleToolCall: req.ParallelToolCalls != nil && !*req.ParallelToolCalls,
		JSONObject:     req.JSONObject(),
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.opt.RequestTimeout)
	defer cancel()
	id := "chatcmpl-" + randHex(12)
	created := time.Now().Unix()
	start := time.Now()
	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		st := newStream(w, id, req.Model, created, s.opt.Keepalive)
		defer st.stop()
		out, p := s.mgr.Handle(ctx, rq, st)
		if p != nil {
			s.logFailure(p, start)
			if p.Sent || st.committed() {
				st.fail(p.Err)
				return
			}
			st.stop()
			writeError(w, p.Err)
			return
		}
		st.finish(out, includeUsage)
		s.logDone(out, start)
		return
	}
	col := &collector{}
	out, p := s.mgr.Handle(ctx, rq, col)
	if p != nil {
		s.logFailure(p, start)
		writeError(w, p.Err)
		return
	}
	writeJSON(w, http.StatusOK, completion(id, req.Model, created, out))
	s.logDone(out, start)
}

func (s *Server) logFailure(p *session.Partial, start time.Time) {
	s.log.Warn("chat completion failed", slog.Int("status", p.Err.Status), slog.String("error", p.Err.Message),
		slog.Bool("partial", p.Sent), slog.Int64("elapsed_ms", time.Since(start).Milliseconds()))
}

func (s *Server) logDone(out *session.Outcome, start time.Time) {
	attrs := []any{slog.String("finish_reason", out.FinishReason), slog.Int("tool_calls", len(out.ToolCalls)), slog.Int64("elapsed_ms", time.Since(start).Milliseconds())}
	if out.Usage != nil {
		attrs = append(attrs, slog.Int("prompt_tokens", out.Usage.PromptTokens), slog.Int("completion_tokens", out.Usage.CompletionTokens))
	}
	s.log.Info("chat completion", attrs...)
}

func completion(id, model string, created int64, out *session.Outcome) openai.ChatCompletion {
	msg := openai.Message{Role: "assistant", ReasoningContent: out.Reasoning}
	if out.Content != "" || len(out.ToolCalls) == 0 {
		msg.Content = openai.TextContent(out.Content)
	}
	for _, c := range out.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{ID: c.ID, Type: "function", Function: openai.FunctionCall{Name: c.Name, Arguments: c.Args}})
	}
	return openai.ChatCompletion{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []openai.Choice{{Index: 0, Message: msg, FinishReason: out.FinishReason}},
		Usage:   out.Usage,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *openai.Error) {
	w.Header().Set("Content-Type", "application/json")
	if ra := e.RetryAfter(); ra != "" {
		w.Header().Set("Retry-After", ra)
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.MarshalEnvelope()) //nolint:gosec // G705: a JSON error body served as application/json, not HTML.
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
