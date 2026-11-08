package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sujoshua/agy-gateway/internal/acp"
	"github.com/sujoshua/agy-gateway/internal/openai"
)

// The sign-in page lets an operator sign the runtime in from a browser,
// wherever the gateway runs: the runtime's OAuth redirect goes to a
// loopback port inside the gateway's host, so the operator pastes the
// redirect address into the page and the gateway delivers it there.
var (
	//go:embed login.html
	loginHTML []byte
	//go:embed login.js
	loginJS []byte
)

func (s *Server) loginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(loginHTML)
	})
	mux.HandleFunc("GET /login.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(loginJS)
	})
	mux.HandleFunc("POST /login/start", s.auth(s.loginStart))
	mux.HandleFunc("POST /login/finish", s.auth(s.loginFinish))
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	u, err := s.rt.StartLogin(ctx)
	if err != nil {
		writeError(w, loginError(err))
		return
	}
	s.log.Info("sign-in started")
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": u})
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request, _ string) {
	var body struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.CallbackURL == "" {
		writeError(w, openai.InvalidRequest("callback_url", "send {\"callback_url\": \"http://127.0.0.1:...\"}"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	tier, err := s.rt.FinishLogin(ctx, body.CallbackURL)
	if err != nil {
		writeError(w, loginError(err))
		return
	}
	s.log.Info("signed in", "tier", tier)
	writeJSON(w, http.StatusOK, map[string]any{"signed_in": true, "tier": tier})
}

func loginError(err error) *openai.Error {
	switch {
	case errors.Is(err, acp.ErrSignedIn):
		return openai.Conflict("already_signed_in", "%v", err)
	case errors.Is(err, acp.ErrNoLogin):
		return openai.Conflict("no_sign_in", "%v", err)
	case errors.Is(err, acp.ErrBadCallback):
		return openai.InvalidRequest("callback_url", "%v", err)
	case errors.Is(err, acp.ErrNotReady):
		return openai.Unavailable("%v", err)
	default:
		return openai.Upstream("sign-in failed: %v", err)
	}
}
