package app_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sujoshua/agy-gateway/internal/acptest"
)

// call sends a request and decodes the JSON reply.
func (e *env) call(method, path, apiKey string, body any) (status int, reply map[string]any) {
	e.t.Helper()
	resp := e.do(method, path, apiKey, body)
	defer closeBody(resp)
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, reply
}

func errorCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestSignInOverHTTP(t *testing.T) {
	agent := acptest.New()
	agent.SignOut()
	e := startEnv(t, nil, agent)
	eventually(t, 5*time.Second, "runtime started", func() bool {
		_, status := e.app.Runtime.Ready()
		return strings.HasPrefix(status, "model discovery failed")
	})

	resp := e.do(http.MethodGet, "/readyz", "", nil)
	closeBody(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz signed out: %d", resp.StatusCode)
	}
	resp = e.do(http.MethodGet, "/login", "", nil)
	closeBody(resp)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("login page: %d %v", resp.StatusCode, resp.Header)
	}
	resp = e.do(http.MethodPost, "/login/start", "", nil)
	closeBody(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("start without a key: %d", resp.StatusCode)
	}
	if code, m := e.call(http.MethodPost, "/login/finish", key, map[string]string{"callback_url": "http://127.0.0.1:1/?code=x"}); code != http.StatusConflict || errorCode(m) != "no_sign_in" {
		t.Fatalf("finish before start: %d %v", code, m)
	}

	code, m := e.call(http.MethodPost, "/login/start", key, nil)
	authURL, _ := m["auth_url"].(string)
	if code != http.StatusOK || !strings.HasPrefix(authURL, "https://accounts.google.com/") {
		t.Fatalf("start: %d %v", code, m)
	}
	if _, again := e.call(http.MethodPost, "/login/start", key, nil); again["auth_url"] != authURL || agent.Authentications() != 1 {
		t.Fatalf("second start should reuse the pending sign-in: %v, %d authenticate calls", again, agent.Authentications())
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	redirect := u.Query().Get("redirect_uri")
	for _, bad := range []string{"https://example.com/?code=x", "http://192.0.2.1:80/?code=x", "not a url"} {
		resp = e.do(http.MethodPost, "/login/finish", key, map[string]string{"callback_url": bad})
		closeBody(resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("finish with %q: %d", bad, resp.StatusCode)
		}
	}
	code, m = e.call(http.MethodPost, "/login/finish", key, map[string]string{"callback_url": redirect + "?code=abc&state=s"})
	if code != http.StatusOK || m["tier"] != "Mock Tier" {
		t.Fatalf("finish: %d %v", code, m)
	}
	if ok, status := e.app.Runtime.Ready(); !ok {
		t.Fatalf("not ready after sign-in: %s", status)
	}
	if code, m := e.call(http.MethodPost, "/login/start", key, nil); code != http.StatusConflict || errorCode(m) != "already_signed_in" {
		t.Fatalf("start when signed in: %d %v", code, m)
	}
	if agent.Starts() != 1 {
		t.Fatalf("sign-in restarted the runtime: %d starts", agent.Starts())
	}
}

func TestSignInRefusedWhenSignedIn(t *testing.T) {
	e := newEnv(t, nil)
	if code, m := e.call(http.MethodPost, "/login/start", key, nil); code != http.StatusConflict || errorCode(m) != "already_signed_in" {
		t.Fatalf("start: %d %v", code, m)
	}
	if e.agent.Authentications() != 0 {
		t.Fatal("authenticate was called on a signed-in runtime")
	}
}
