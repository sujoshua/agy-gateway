// Package app wires the runtime supervisor, tool bridge, session manager
// and HTTP server into one gateway.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/acp"
	"github.com/sujoshua/agy-gateway/internal/bridge"
	"github.com/sujoshua/agy-gateway/internal/config"
	"github.com/sujoshua/agy-gateway/internal/harness"
	"github.com/sujoshua/agy-gateway/internal/server"
	"github.com/sujoshua/agy-gateway/internal/session"
)

// App is a gateway instance.
type App struct {
	Runtime  *acp.Runtime
	Sessions *session.Manager
	Bridge   *bridge.Bridge

	cfg *config.Config
	log *slog.Logger
	srv *server.Server
	// Harness says which localharness the runtime uses: "official",
	// "custom", "patched", or "patch failed" (the official one is used).
	Harness string

	bridgeLn  net.Listener
	bridgeURL string
}

// New builds a gateway. start launches the runtime; nil runs cfg.RuntimeCmd.
// The manager's turns run on ctx.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, version string, start acp.Starter) (*App, error) {
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("work dir: %w", err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.BridgeListen)
	if err != nil {
		return nil, fmt.Errorf("bridge listener: %w", err)
	}
	a := &App{cfg: cfg, log: log, bridgeLn: ln, bridgeURL: "http://" + ln.Addr().String(), Harness: "official"}
	if start == nil {
		var env []string
		if path := a.prepareHarness(ctx); path != "" {
			env = append(env, harness.EnvVar+"="+path)
		}
		start = acp.ExecStarter(acp.ExecOptions{
			Cmd: cfg.RuntimeCmd, Args: cfg.RuntimeArgs, Home: cfg.RuntimeHome, Dir: cfg.WorkDir, Env: env,
			Logger: log.With(slog.String("component", "runtime")),
		})
	}
	a.Bridge = bridge.New(bridge.Options{
		ResultTimeout: cfg.ToolResultTimeout,
		SchemaBudget:  cfg.SchemaBudget,
		Logger:        log.With(slog.String("component", "bridge")),
	})
	a.Runtime = acp.New(acp.Options{
		Start:         start,
		WorkDir:       cfg.WorkDir,
		StateDir:      StateDir(cfg),
		MCPServerName: cfg.MCPServerName,
		Version:       version,
		Logger:        log.With(slog.String("component", "acp")),
		OnUpdate:      func(n sdk.SessionNotification) { a.Sessions.Dispatch(n) },
		OnRestart:     func(err error) { a.Sessions.RuntimeRestarted(err) },
	})
	a.Sessions = session.NewManager(ctx, session.Options{
		Runtime:       a.Runtime,
		Bridge:        a.Bridge,
		BridgeURL:     func() string { return a.bridgeURL },
		MCPServerName: cfg.MCPServerName,
		TTL:           cfg.SessionTTL,
		MaxSessions:   cfg.MaxSessions,
		BatchWindow:   cfg.ToolBatchWindow,
		ReplayBudget:  cfg.ReplayBudget,
		Logger:        log.With(slog.String("component", "session")),
	})
	a.srv = server.New(server.Options{
		APIKeys:           cfg.APIKeys,
		RequestTimeout:    cfg.RequestTimeout,
		IgnoreUnsupported: cfg.IgnoreUnsupported,
		EmitReasoning:     cfg.EmitReasoning,
		Harness:           a.Harness,
		Logger:            log.With(slog.String("component", "http")),
	}, a.Runtime, a.Sessions)
	return a, nil
}

// prepareHarness returns the harness path to hand to the runtime, or ""
// to let it use the one next to itself. A failed patch is logged and falls
// back to the unpatched harness so the gateway still serves.
func (a *App) prepareHarness(ctx context.Context) string {
	cfg := a.cfg
	if len(cfg.HarnessPatchCmd) == 0 {
		if cfg.HarnessPath != "" {
			a.Harness = "custom"
		}
		return cfg.HarnessPath
	}
	log := a.log.With(slog.String("component", "harness"))
	path, err := harness.Prepare(ctx, harness.Options{
		Source: cfg.HarnessPath, RuntimeCmd: cfg.RuntimeCmd, PatchCmd: cfg.HarnessPatchCmd,
		CacheDir: cfg.HarnessCacheDir, Logger: log,
	})
	if err != nil {
		a.Harness = "patch failed"
		log.Error("harness patch failed; using the unpatched harness", slog.Any("err", err))
		return cfg.HarnessPath
	}
	a.Harness = "patched"
	log.Info("using patched harness", slog.String("path", path))
	return path
}

// StateDir is the runtime's state directory for cfg.
func StateDir(cfg *config.Config) string {
	home := cfg.RuntimeHome
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-acp")
}

// Handler is the public OpenAI-compatible API.
func (a *App) Handler() http.Handler { return a.srv.Handler() }

// BridgeURL is the loopback URL of the MCP tool bridge.
func (a *App) BridgeURL() string { return a.bridgeURL }

// Run runs the runtime, the session sweeper and the bridge until ctx ends,
// then closes all sessions and stops the runtime.
func (a *App) Run(ctx context.Context) error {
	runtimeDone := make(chan struct{})
	go func() { defer close(runtimeDone); a.Runtime.Run(ctx) }()
	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); a.Sessions.Run(ctx) }()
	bridgeSrv := &http.Server{Handler: a.Bridge, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- bridgeSrv.Serve(a.bridgeLn) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	<-mgrDone
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = bridgeSrv.Shutdown(shutdown)
	<-runtimeDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
