// Package acp supervises the official Antigravity ACP runtime process and
// speaks the Agent Client Protocol to it through the coder/acp-go-sdk.
package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// ClientName is the honest client identity sent in initialize.
const ClientName = "agy-gateway"

// ErrNotReady is returned while the runtime is starting, restarting, or not
// signed in.
var ErrNotReady = errors.New("runtime is not ready")

// Process is a running runtime: its stdio and lifecycle hooks.
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	// Done is closed when the process has exited.
	Done <-chan struct{}
	// Kill stops the process; it must be safe to call more than once.
	Kill func()
	// AuthURLs receives the sign-in URL the runtime prints during
	// authenticate. Nil when the process cannot report it.
	AuthURLs <-chan string
}

// Starter launches a runtime process.
type Starter func(ctx context.Context) (*Process, error)

// Model is one exposed model id and the session config it maps to.
type Model struct {
	ID            string
	Value         string // value of the model config option
	ThinkingLevel string // value of the thought_level option, "" for default
}

// Options configures a Runtime.
type Options struct {
	Start Starter
	// WorkDir is where per-session working directories are created.
	WorkDir string
	// StateDir is the runtime state directory (~/.gemini/antigravity-acp),
	// used only to purge conversation files of closed sessions. Empty
	// disables purging.
	StateDir string
	// MCPServerName is the only MCP server whose tool calls are approved.
	MCPServerName string
	Version       string
	Logger        *slog.Logger
	// OnUpdate receives every session/update notification.
	OnUpdate func(sdk.SessionNotification)
	// OnRestart is called after the runtime process died, before restart.
	OnRestart func(error)
}

// Runtime supervises the runtime process and exposes its connection.
type Runtime struct {
	opt Options
	log *slog.Logger

	mu       sync.RWMutex
	conn     *sdk.ClientSideConnection
	authURLs <-chan string
	login    *pendingLogin
	ready    bool
	status   string
	models   []Model
	modelOpt string // config option id for the model
	levelOpt string // config option id for the thought level
	tier     string

	// loginMu serializes StartLogin and FinishLogin.
	loginMu sync.Mutex
}

// New creates a Runtime; call Run to start it.
func New(opt Options) *Runtime {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	return &Runtime{opt: opt, log: opt.Logger, status: "starting"}
}

// Run starts the runtime and restarts it after crashes until ctx ends.
func (r *Runtime) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := r.runOnce(ctx)
		r.setDown(fmt.Sprintf("runtime exited: %v", err))
		if ctx.Err() != nil {
			return
		}
		r.log.Error("runtime exited, restarting", slog.Any("err", err), slog.Duration("backoff", backoff))
		if r.opt.OnRestart != nil {
			r.opt.OnRestart(err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (r *Runtime) runOnce(ctx context.Context) error {
	proc, err := r.opt.Start(ctx)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer proc.Kill()
	conn := sdk.NewClientSideConnection(&client{r: r}, proc.Stdin, proc.Stdout)
	conn.SetLogger(slog.New(RedactingHandler{r.log.Handler()}).With(slog.String("component", "acp-sdk")))

	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	init, err := conn.Initialize(initCtx, sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{},
		ClientInfo:         &sdk.Implementation{Name: ClientName, Version: r.opt.Version},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if !init.AgentCapabilities.McpCapabilities.Http {
		return errors.New("runtime does not support HTTP MCP servers")
	}
	r.mu.Lock()
	r.conn = conn
	r.authURLs = proc.AuthURLs
	r.status = "discovering models"
	r.mu.Unlock()

	go r.discoverLoop(ctx, conn)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-proc.Done:
		return errors.New("process exited")
	case <-conn.Done():
		return errors.New("connection closed")
	}
}

// discoverLoop creates a throwaway session to learn the models; it retries
// while the runtime is not signed in.
func (r *Runtime) discoverLoop(ctx context.Context, conn *sdk.ClientSideConnection) {
	delay := 5 * time.Second
	for {
		if r.readyOn(conn) {
			return
		}
		err := r.discover(ctx, conn)
		if err == nil {
			return
		}
		r.mu.Lock()
		if r.conn != conn {
			r.mu.Unlock()
			return
		}
		r.status = "model discovery failed: " + err.Error()
		r.mu.Unlock()
		r.log.Warn("model discovery failed (if the runtime is not signed in, open /login or run agy-gateway login)", slog.Any("err", err))
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Minute)
	}
}

func (r *Runtime) discover(ctx context.Context, conn *sdk.ClientSideConnection) error {
	dir, err := r.NewWorkDir()
	if err != nil {
		return err
	}
	defer r.removeWorkDir(dir)
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := conn.NewSession(cctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		return err
	}
	defer r.CloseSession(context.WithoutCancel(ctx), string(resp.SessionId))
	models, modelOpt, levelOpt := modelsFromConfig(resp.ConfigOptions)
	if len(models) == 0 {
		return errors.New("runtime reported no models")
	}
	tier := ""
	if t, ok := dig(resp.Meta, "antigravity", "tier", "name").(string); ok {
		tier = t
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != conn {
		return nil
	}
	r.models, r.modelOpt, r.levelOpt, r.tier = models, modelOpt, levelOpt, tier
	r.ready = true
	r.status = "ready"
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	r.log.Info("runtime ready", slog.Any("models", ids), slog.String("tier", tier))
	return nil
}

func modelsFromConfig(opts []sdk.SessionConfigOption) (models []Model, modelOpt, levelOpt string) {
	var values, levels []string
	for _, o := range opts {
		s := o.Select
		if s == nil || s.Category == nil {
			continue
		}
		var vals []string
		if s.Options.Ungrouped != nil {
			for _, v := range *s.Options.Ungrouped {
				vals = append(vals, string(v.Value))
			}
		}
		if s.Options.Grouped != nil {
			for _, g := range *s.Options.Grouped {
				for _, v := range g.Options {
					vals = append(vals, string(v.Value))
				}
			}
		}
		switch *s.Category {
		case sdk.SessionConfigOptionCategoryModel:
			modelOpt, values = string(s.Id), vals
		case sdk.SessionConfigOptionCategoryThoughtLevel:
			levelOpt, levels = string(s.Id), vals
		default:
		}
	}
	for _, v := range values {
		models = append(models, Model{ID: v, Value: v})
		for _, l := range levels {
			models = append(models, Model{ID: v + "-" + l, Value: v, ThinkingLevel: l})
		}
	}
	return models, modelOpt, levelOpt
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func (r *Runtime) readyOn(conn *sdk.ClientSideConnection) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conn == conn && r.ready
}

func (r *Runtime) setDown(status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conn = nil
	r.authURLs = nil
	r.login = nil
	r.ready = false
	r.status = status
}

// Conn returns the live connection, or ErrNotReady.
func (r *Runtime) Conn() (*sdk.ClientSideConnection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready || r.conn == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotReady, r.status)
	}
	return r.conn, nil
}

// Ready reports readiness and a human-readable status.
func (r *Runtime) Ready() (ok bool, status string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready, r.status
}

// Tier is the account tier the runtime reported, "" before discovery.
func (r *Runtime) Tier() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tier
}

// Models returns the discovered models.
func (r *Runtime) Models() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Model(nil), r.models...)
}

// FindModel resolves an exposed model id; reasoningEffort, when non-empty,
// picks the thinking level for an id without one.
func (r *Runtime) FindModel(id, reasoningEffort string) (Model, bool) {
	for _, m := range r.Models() {
		if m.ID != id {
			continue
		}
		if m.ThinkingLevel == "" && reasoningEffort != "" {
			for _, l := range r.Models() {
				if l.Value == m.Value && l.ThinkingLevel == reasoningEffort {
					return l, true
				}
			}
		}
		return m, true
	}
	return Model{}, false
}

// ApplyModel sets the session's model and thinking level.
func (r *Runtime) ApplyModel(ctx context.Context, sessionID string, m Model) error {
	conn, err := r.Conn()
	if err != nil {
		return err
	}
	r.mu.RLock()
	modelOpt, levelOpt := r.modelOpt, r.levelOpt
	r.mu.RUnlock()
	set := func(id, value string) error {
		_, err := conn.SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{
			SessionId: sdk.SessionId(sessionID),
			ConfigId:  sdk.SessionConfigId(id),
			Value:     sdk.SessionConfigValueId(value),
		}})
		return err
	}
	if modelOpt != "" {
		if err := set(modelOpt, m.Value); err != nil {
			return fmt.Errorf("set model: %w", err)
		}
	}
	if levelOpt != "" && m.ThinkingLevel != "" {
		if err := set(levelOpt, m.ThinkingLevel); err != nil {
			return fmt.Errorf("set thinking level: %w", err)
		}
	}
	return nil
}

// NewWorkDir creates an empty private working directory for one session, so
// the runtime's own file tools never see anything of the host.
func (r *Runtime) NewWorkDir() (string, error) {
	if err := os.MkdirAll(r.opt.WorkDir, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(r.opt.WorkDir, "s-")
}

func (r *Runtime) removeWorkDir(dir string) {
	if dir != "" && strings.HasPrefix(dir, r.opt.WorkDir) {
		_ = os.RemoveAll(dir)
	}
}

// CloseSession closes a session, best effort, and purges its stored
// conversation from the runtime state directory.
func (r *Runtime) CloseSession(ctx context.Context, sessionID string) {
	if conn, err := r.Conn(); err == nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := conn.CloseSession(cctx, sdk.CloseSessionRequest{SessionId: sdk.SessionId(sessionID)}); err != nil {
			r.log.Debug("session/close failed", slog.String("session", sessionID), slog.Any("err", err))
		}
		cancel()
	}
	r.purge(sessionID)
}

// RemoveWorkDir deletes a directory made by NewWorkDir.
func (r *Runtime) RemoveWorkDir(dir string) { r.removeWorkDir(dir) }

func (r *Runtime) purge(sessionID string) {
	if r.opt.StateDir == "" || !safeID(sessionID) {
		return
	}
	for _, suffix := range []string{".db", ".db-shm", ".db-wal", ".meta"} {
		_ = os.Remove(filepath.Join(r.opt.StateDir, "conversations", sessionID+suffix))
	}
	_ = os.RemoveAll(filepath.Join(r.opt.StateDir, "brain", sessionID))
}

func safeID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// Cancel sends session/cancel.
func (r *Runtime) Cancel(ctx context.Context, sessionID string) {
	conn, err := r.Conn()
	if err != nil {
		return
	}
	if err := conn.Cancel(ctx, sdk.CancelNotification{SessionId: sdk.SessionId(sessionID)}); err != nil {
		r.log.Debug("session/cancel failed", slog.String("session", sessionID), slog.Any("err", err))
	}
}
