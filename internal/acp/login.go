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

// Login signs the runtime in through its own OAuth flow. The runtime prints
// the authorize URL on stderr (the starter must pass stderr through) and
// waits for the browser redirect on a loopback port. callback returns the
// redirect URL the operator copied from the browser; Login delivers it to
// that loopback port. Credentials stay inside the runtime's state directory;
// the gateway never reads them. It returns the account tier.
func Login(ctx context.Context, opt Options, callback func(context.Context) (string, error)) (string, error) {
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
	r.conn = conn
	r.mu.Unlock()
	if err := r.discover(ctx, conn); err == nil {
		return r.tier, errAlreadySignedIn
	}

	authDone := make(chan error, 1)
	go func() {
		_, err := conn.Authenticate(ctx, sdk.AuthenticateRequest{MethodId: LoginMethod})
		authDone <- err
	}()
	cbURL, err := callback(ctx)
	if err != nil {
		return "", err
	}
	if err := deliverCallback(ctx, cbURL); err != nil {
		return "", err
	}
	select {
	case err := <-authDone:
		if err != nil {
			return "", fmt.Errorf("authenticate: %w", err)
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := r.discover(ctx, conn); err != nil {
		return "", fmt.Errorf("signed in, but creating a session failed: %w", err)
	}
	return r.tier, nil
}

var errAlreadySignedIn = errors.New("the runtime is already signed in")

// IsAlreadySignedIn reports whether Login found an existing sign-in and
// changed nothing.
func IsAlreadySignedIn(err error) bool { return errors.Is(err, errAlreadySignedIn) }

// deliverCallback forwards the OAuth redirect to the runtime's loopback
// listener. Only http://127.0.0.1:<port>/... URLs are accepted, so the
// gateway never sends the authorization code anywhere else.
func deliverCallback(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("the callback is not a URL")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if u.Scheme != "http" || err != nil || (host != "127.0.0.1" && host != "localhost") || port == "" {
		return errors.New("the callback must be the http://127.0.0.1:<port>/... (or localhost) URL the browser was redirected to")
	}
	u.Host = net.JoinHostPort("127.0.0.1", port)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Do not echo the URL: it carries the authorization code.
		return errors.New("could not reach the runtime's callback listener; was the URL from this login attempt?")
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("the runtime rejected the callback (HTTP %d)", resp.StatusCode)
	}
	return nil
}
