package acp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// LoginMethod is the runtime's personal Google account sign-in.
const LoginMethod = "oauth-personal"

var (
	// ErrSignedIn is returned by StartLogin when the runtime can already
	// create sessions; nothing is changed.
	ErrSignedIn = errors.New("the runtime is already signed in")
	// ErrNoLogin is returned by FinishLogin without a sign-in in progress.
	ErrNoLogin = errors.New("no sign-in is in progress; start one first")
	// ErrBadCallback is returned by FinishLogin for an address that is not
	// the runtime's loopback redirect.
	ErrBadCallback = errors.New("the callback must be the http://127.0.0.1:<port>/... (or localhost) URL the browser was redirected to")
)

// loginTimeout bounds how long a started sign-in waits for its callback.
const loginTimeout = 10 * time.Minute

// pendingLogin is an authenticate request waiting for the browser redirect.
type pendingLogin struct {
	conn     *sdk.ClientSideConnection
	url      string
	finished chan struct{}
	err      error // valid once finished is closed
}

// StartLogin starts the runtime's own OAuth sign-in on the live connection
// and returns the Google authorize URL; a sign-in already in progress
// returns its URL again. The runtime then waits for the browser redirect on
// a loopback port inside its own network namespace, which FinishLogin
// reaches. It never signs out: if the runtime can already create a session
// it returns ErrSignedIn. Credentials stay in the runtime's state directory;
// the gateway never reads them.
func (r *Runtime) StartLogin(ctx context.Context) (string, error) {
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	r.mu.RLock()
	conn, ready, urls, p := r.conn, r.ready, r.authURLs, r.login
	r.mu.RUnlock()
	switch {
	case conn == nil:
		return "", fmt.Errorf("%w: the runtime is not running", ErrNotReady)
	case ready:
		return "", ErrSignedIn
	case p != nil && p.conn == conn && !p.done():
		return p.url, nil
	case urls == nil:
		return "", errors.New("this runtime process does not report its sign-in URL")
	}
	if err := r.discover(ctx, conn); err == nil {
		return "", ErrSignedIn
	}
	select {
	case <-urls: // a URL left over from an earlier attempt
	default:
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
	p = &pendingLogin{conn: conn, finished: make(chan struct{})}
	go func() {
		defer cancel()
		_, p.err = conn.Authenticate(actx, sdk.AuthenticateRequest{MethodId: LoginMethod})
		close(p.finished)
	}()
	select {
	case p.url = <-urls:
	case <-p.finished:
		if p.err != nil {
			return "", fmt.Errorf("authenticate: %w", p.err)
		}
		return "", ErrSignedIn
	case <-time.After(time.Minute):
		cancel()
		return "", errors.New("the runtime did not print a sign-in URL")
	case <-ctx.Done():
		cancel()
		return "", ctx.Err()
	}
	r.mu.Lock()
	if r.conn == conn {
		r.login = p
	}
	r.mu.Unlock()
	return p.url, nil
}

// FinishLogin delivers the address the browser was redirected to after
// StartLogin, waits for the runtime to finish signing in, and discovers the
// models. It returns the account tier.
func (r *Runtime) FinishLogin(ctx context.Context, callback string) (string, error) {
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	r.mu.RLock()
	conn, p := r.conn, r.login
	r.mu.RUnlock()
	if p == nil || p.conn != conn || p.done() {
		return "", ErrNoLogin
	}
	if err := deliverCallback(ctx, callback); err != nil {
		return "", err
	}
	select {
	case <-p.finished:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	r.mu.Lock()
	if r.login == p {
		r.login = nil
	}
	r.mu.Unlock()
	if p.err != nil {
		return "", fmt.Errorf("authenticate: %w", p.err)
	}
	if err := r.discover(ctx, conn); err != nil {
		return "", fmt.Errorf("signed in, but creating a session failed: %w", err)
	}
	return r.Tier(), nil
}

func (p *pendingLogin) done() bool {
	select {
	case <-p.finished:
		return true
	default:
		return false
	}
}

// Login signs in a runtime process of its own, for the login command.
// callback receives the authorize URL and returns the redirect URL the
// operator copied from the browser. It returns the account tier, and
// ErrSignedIn if the runtime was already signed in.
func Login(ctx context.Context, opt Options, callback func(ctx context.Context, authURL string) (string, error)) (string, error) {
	r := New(opt)
	proc, err := opt.Start(ctx)
	if err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	defer proc.Kill()
	conn := sdk.NewClientSideConnection(&client{r: r}, proc.Stdin, proc.Stdout)
	conn.SetLogger(r.log)
	if _, err := conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &sdk.Implementation{Name: ClientName, Version: opt.Version},
	}); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	r.mu.Lock()
	r.conn, r.authURLs = conn, proc.AuthURLs
	r.mu.Unlock()
	authURL, err := r.StartLogin(ctx)
	if err != nil {
		return r.Tier(), err
	}
	cb, err := callback(ctx, authURL)
	if err != nil {
		return "", err
	}
	return r.FinishLogin(ctx, cb)
}

// deliverCallback forwards the OAuth redirect to the runtime's loopback
// listener. Only http://127.0.0.1:<port>/... URLs are accepted, so the
// gateway never sends the authorization code anywhere else.
func deliverCallback(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ErrBadCallback
	}
	host, port, err := net.SplitHostPort(u.Host)
	if u.Scheme != "http" || err != nil || (host != "127.0.0.1" && host != "localhost") || port == "" {
		return ErrBadCallback
	}
	u.Host = net.JoinHostPort("127.0.0.1", port)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return err
	}
	// Do not follow the runtime's redirect to its "signed in" page.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		// Do not echo the URL: it carries the authorization code.
		return errors.New("could not reach the runtime's callback listener; was the URL from this sign-in attempt?")
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("the runtime rejected the callback (HTTP %d)", resp.StatusCode)
	}
	return nil
}
