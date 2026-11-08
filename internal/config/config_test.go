package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.ToolResultTimeout != 170*time.Second || len(c.APIKeys) != 0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"AGY_GW_API_KEYS":     " a , b ,,",
		"AGY_GW_RUNTIME_ARGS": "--uid= --x",
		"AGY_GW_SESSION_TTL":  "5m",
		"AGY_GW_MAX_SESSIONS": "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIKeys) != 2 || c.APIKeys[1] != "b" {
		t.Fatalf("keys: %q", c.APIKeys)
	}
	if len(c.RuntimeArgs) != 2 || c.SessionTTL != 5*time.Minute || c.MaxSessions != 3 {
		t.Fatalf("overrides: %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"AGY_GW_SESSION_TTL": "soon"},
		{"AGY_GW_MAX_SESSIONS": "-1"},
		{"AGY_GW_TOOL_RESULT_TIMEOUT": "3m"},
		{"AGY_GW_MCP_SERVER_NAME": "a b"},
		{"AGY_GW_LOG_LEVEL": "loud"},
		{"AGY_GW_EMIT_REASONING": "maybe"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%v: expected error", m)
		}
	}
}
