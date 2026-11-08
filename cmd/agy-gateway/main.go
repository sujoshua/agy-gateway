// Command agy-gateway serves an OpenAI-compatible HTTP API backed by the
// official Antigravity ACP runtime.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sujoshua/agy-gateway/internal/acp"
	"github.com/sujoshua/agy-gateway/internal/app"
	"github.com/sujoshua/agy-gateway/internal/config"
	"github.com/sujoshua/agy-gateway/internal/harness"
)

var version = "dev"

const usage = `usage: agy-gateway [serve|login|version]

  serve    run the gateway (default); configured by AGY_GW_* variables
  login    sign the runtime in to a Google account (interactive)
  version  print the version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agy-gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "serve", "login":
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := newLogger(cfg.LogLevel)
	if cmd == "login" {
		return login(ctx, cfg, log)
	}
	return serve(ctx, cfg, log)
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	a, err := app.New(ctx, cfg, log, version, nil)
	if err != nil {
		return err
	}
	if len(cfg.APIKeys) == 0 {
		log.Warn("AGY_GW_API_KEYS is empty: the API accepts any caller")
	}
	appCtx, stopApp := context.WithCancel(context.WithoutCancel(ctx))
	appDone := make(chan error, 1)
	go func() { appDone <- a.Run(appCtx) }()

	public := &http.Server{Addr: cfg.Listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- public.ListenAndServe() }()
	log.Info("agy-gateway listening", slog.String("listen", cfg.Listen), slog.String("bridge", a.BridgeURL()), slog.String("version", version))

	select {
	case <-ctx.Done():
	case err = <-errc:
		log.Error("server stopped", slog.Any("err", err))
	}
	// Stop taking requests first, then close sessions and the runtime.
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdown)
	stopApp()
	if aerr := <-appDone; aerr != nil {
		log.Error("shutdown", slog.Any("err", aerr))
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func login(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return err
	}
	cmd, err := app.RuntimeCmd(ctx, cfg, log)
	if err != nil {
		return err
	}
	opt := acp.Options{
		Start: acp.ExecStarter(acp.ExecOptions{
			Cmd: cmd, Args: cfg.RuntimeArgs, Home: cfg.RuntimeHome, Dir: cfg.WorkDir, Env: harnessEnv(cfg),
			Logger: log.With(slog.String("component", "runtime")),
		}),
		WorkDir:       cfg.WorkDir,
		MCPServerName: cfg.MCPServerName,
		Version:       version,
		Logger:        log,
	}
	tier, err := acp.Login(ctx, opt, func(ctx context.Context, authURL string) (string, error) {
		fmt.Fprintf(os.Stderr, "Open this URL and sign in:\n\n%s\n\nThe browser then fails to load an http://127.0.0.1:... address. Paste that address here:\n", authURL)
		return readLine(ctx, os.Stdin)
	})
	if errors.Is(err, acp.ErrSignedIn) {
		fmt.Fprintf(os.Stderr, "The runtime is already signed in (tier %q); nothing changed.\n", tier)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Signed in (tier %q).\n", tier)
	return nil
}

func readLine(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil && line == "" {
			ch <- result{err: err}
			return
		}
		ch <- result{line: line}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// harnessEnv points the login runtime at an explicitly configured harness.
// Sign-in does not need a patched one.
func harnessEnv(cfg *config.Config) []string {
	if cfg.HarnessPath == "" {
		return nil
	}
	return []string{harness.EnvVar + "=" + cfg.HarnessPath}
}
