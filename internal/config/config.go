// Package config loads gateway settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable of the gateway. Values come from AGY_GW_*
// environment variables; see README.md for the full table.
type Config struct {
	// Listen is the public OpenAI-compatible HTTP address.
	Listen string
	// BridgeListen is the loopback address of the MCP tool bridge that only
	// the runtime talks to. Port 0 picks a free port.
	BridgeListen string
	// APIKeys, when non-empty, are the bearer tokens the gateway accepts.
	APIKeys []string

	// RuntimeCmd and RuntimeArgs start the official ACP runtime.
	RuntimeCmd  string
	RuntimeArgs []string
	// RuntimeHome overrides HOME for the runtime so its state directory
	// (~/.gemini/antigravity-acp) can live on a volume. Empty keeps HOME.
	RuntimeHome string
	// WorkDir holds one empty working directory per ACP session.
	WorkDir string
	// HarnessPath, when set, is the localharness_external the runtime uses
	// instead of the one next to RuntimeCmd.
	HarnessPath string
	// HarnessPatchCmd, when set, patches a copy of the harness before the
	// runtime starts (for example a context-window unlock). Off by default.
	HarnessPatchCmd []string
	// HarnessCacheDir keeps patched copies across restarts.
	HarnessCacheDir string

	SessionTTL        time.Duration
	MaxSessions       int
	ToolResultTimeout time.Duration
	RequestTimeout    time.Duration
	ToolBatchWindow   time.Duration
	ReplayBudget      int
	SchemaBudget      int
	MCPServerName     string

	// IgnoreUnsupported downgrades unsupported sampling parameters
	// (temperature, top_p, max_tokens, ...) from a 400 error to a no-op.
	IgnoreUnsupported bool
	// EmitReasoning streams runtime thoughts as reasoning_content.
	EmitReasoning bool
	LogLevel      string
}

// Load reads the configuration from getenv (os.Getenv in production).
func Load(getenv func(string) string) (*Config, error) {
	c := &Config{
		Listen:            ":8080",
		BridgeListen:      "127.0.0.1:0",
		RuntimeCmd:        "agy_acp_server.par",
		RuntimeArgs:       []string{"--uid="},
		WorkDir:           filepath.Join(os.TempDir(), "agy-gateway"),
		SessionTTL:        30 * time.Minute,
		MaxSessions:       64,
		ToolResultTimeout: 170 * time.Second,
		RequestTimeout:    15 * time.Minute,
		ToolBatchWindow:   250 * time.Millisecond,
		ReplayBudget:      160 * 1024,
		SchemaBudget:      1350,
		MCPServerName:     "client",
		EmitReasoning:     true,
		LogLevel:          "info",
	}
	var errs []error
	str := func(key string, dst *string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive duration, got %q", key, v))
			return
		}
		*dst = d
	}
	num := func(key string, dst *int) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive integer, got %q", key, v))
			return
		}
		*dst = n
	}
	boolean := func(key string, dst *bool) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want true or false, got %q", key, v))
			return
		}
		*dst = b
	}

	str("AGY_GW_LISTEN", &c.Listen)
	str("AGY_GW_BRIDGE_LISTEN", &c.BridgeListen)
	str("AGY_GW_RUNTIME_CMD", &c.RuntimeCmd)
	if v, ok := lookup(getenv, "AGY_GW_RUNTIME_ARGS"); ok {
		c.RuntimeArgs = strings.Fields(v)
	}
	str("AGY_GW_RUNTIME_HOME", &c.RuntimeHome)
	str("AGY_GW_WORKDIR", &c.WorkDir)
	str("AGY_GW_HARNESS_PATH", &c.HarnessPath)
	c.HarnessPatchCmd = strings.Fields(getenv("AGY_GW_HARNESS_PATCH_CMD"))
	str("AGY_GW_HARNESS_CACHE_DIR", &c.HarnessCacheDir)
	str("AGY_GW_MCP_SERVER_NAME", &c.MCPServerName)
	str("AGY_GW_LOG_LEVEL", &c.LogLevel)
	dur("AGY_GW_SESSION_TTL", &c.SessionTTL)
	dur("AGY_GW_TOOL_RESULT_TIMEOUT", &c.ToolResultTimeout)
	dur("AGY_GW_REQUEST_TIMEOUT", &c.RequestTimeout)
	dur("AGY_GW_TOOL_BATCH_WINDOW", &c.ToolBatchWindow)
	num("AGY_GW_MAX_SESSIONS", &c.MaxSessions)
	num("AGY_GW_REPLAY_BUDGET", &c.ReplayBudget)
	num("AGY_GW_SCHEMA_BUDGET", &c.SchemaBudget)
	boolean("AGY_GW_IGNORE_UNSUPPORTED_PARAMS", &c.IgnoreUnsupported)
	boolean("AGY_GW_EMIT_REASONING", &c.EmitReasoning)
	for k := range strings.SplitSeq(getenv("AGY_GW_API_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.APIKeys = append(c.APIKeys, k)
		}
	}

	if c.ToolResultTimeout >= 180*time.Second {
		errs = append(errs, errors.New("AGY_GW_TOOL_RESULT_TIMEOUT must stay below the runtime's 180s MCP call limit"))
	}
	if !validServerName(c.MCPServerName) {
		errs = append(errs, fmt.Errorf("AGY_GW_MCP_SERVER_NAME %q: use letters, digits and '-'", c.MCPServerName))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("AGY_GW_LOG_LEVEL %q: want debug, info, warn or error", c.LogLevel))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if c.HarnessCacheDir == "" {
		c.HarnessCacheDir = defaultCacheDir(c)
	}
	return c, nil
}

// defaultCacheDir keeps patched harness copies next to the runtime's state,
// which is a volume in the container image, so restarts reuse them.
func defaultCacheDir(c *Config) string {
	home := c.RuntimeHome
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return filepath.Join(c.WorkDir, "harness")
	}
	return filepath.Join(home, ".cache", "agy-gateway", "harness")
}

func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	return v, v != ""
}

func validServerName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
