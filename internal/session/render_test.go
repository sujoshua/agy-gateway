package session

import (
	"strings"
	"testing"

	"github.com/sujoshua/agy-gateway/internal/openai"
)

func msg(role, text string) openai.Message {
	return openai.Message{Role: role, Content: openai.TextContent(text)}
}

func call(id, name, args string) openai.ToolCall {
	return openai.ToolCall{ID: id, Type: "function", Function: openai.FunctionCall{Name: name, Arguments: args}}
}

func TestFingerprintMatchesEchoes(t *testing.T) {
	a := openai.Message{Role: "assistant", ToolCalls: []openai.ToolCall{call("c1", "f", `{"b":1,"a":2}`)}}
	b := openai.Message{Role: "assistant", Content: openai.TextContent("  "), ToolCalls: []openai.ToolCall{call("c1", "f", `{ "a": 2, "b": 1 }`)}}
	if fingerprint(a) != fingerprint(b) {
		t.Fatal("argument formatting or empty content changed the fingerprint")
	}
	if fingerprint(msg("user", "hi")) == fingerprint(msg("assistant", "hi")) {
		t.Fatal("role is not part of the fingerprint")
	}
	c := a
	c.ToolCalls = []openai.ToolCall{call("c2", "f", `{"a":2,"b":1}`)}
	if fingerprint(a) == fingerprint(c) {
		t.Fatal("call id is not part of the fingerprint")
	}
}

func TestToolsKeyIgnoresOrder(t *testing.T) {
	x := openai.Tool{Type: "function", Function: openai.FunctionDef{Name: "x", Parameters: []byte(`{"type":"object","properties":{}}`)}}
	y := openai.Tool{Type: "function", Function: openai.FunctionDef{Name: "y"}}
	if toolsKey([]openai.Tool{x, y}) != toolsKey([]openai.Tool{y, x}) {
		t.Fatal("order changed the key")
	}
	if toolsKey(nil) != "" || toolsKey([]openai.Tool{x}) == toolsKey([]openai.Tool{y}) {
		t.Fatal("distinct tool sets share a key")
	}
}

func TestTrailingStartAndSystemText(t *testing.T) {
	msgs := []openai.Message{msg("system", "s"), msg("developer", "d"), msg("user", "u1"), msg("assistant", "a"), msg("tool", "r"), msg("user", "u2")}
	if got := trailingStart(msgs); got != 4 {
		t.Fatalf("trailingStart = %d", got)
	}
	if got := systemText(msgs); got != "s\n\nd" {
		t.Fatalf("systemText = %q", got)
	}
	if trailingStart(nil) != 0 {
		t.Fatal("empty")
	}
}

func TestTranscriptBudget(t *testing.T) {
	msgs := []openai.Message{
		msg("user", strings.Repeat("old ", 50)),
		{Role: "assistant", ToolCalls: []openai.ToolCall{call("c1", "get_weather", `{"city":"Paris"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: openai.TextContent("rain")},
		msg("user", "newest"),
	}
	full := transcript(msgs, 1<<20)
	for _, want := range []string{"[user]: old", "[assistant called tool get_weather, call id c1, arguments {\"city\":\"Paris\"}]", "[result of tool get_weather, call id c1]: rain", "[user]: newest"} {
		if !strings.Contains(full, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, full)
		}
	}
	short := transcript(msgs, 150)
	if !strings.HasPrefix(short, "[1 earlier messages omitted]") || !strings.HasSuffix(short, "[user]: newest") || strings.Contains(short, "old") {
		t.Fatalf("budgeted transcript:\n%s", short)
	}
}

func TestPromptBlocks(t *testing.T) {
	img := openai.Message{Role: "user", Content: openai.Content{Set: true, Parts: []openai.Part{
		{Type: "text", Text: "look"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
	}}}
	tool := openai.Message{Role: "tool", ToolCallID: "c1", Content: openai.TextContent("42")}
	blocks := promptBlocks([]openai.Message{img, tool}, map[string]string{"c1": "calc"})
	if len(blocks) != 3 || blocks[0].Text == nil || blocks[1].Image == nil || blocks[1].Image.MimeType != "image/png" {
		t.Fatalf("blocks: %+v", blocks)
	}
	if blocks[2].Text.Text != "[result of tool calc, call id c1]: 42" {
		t.Fatalf("tool block: %q", blocks[2].Text.Text)
	}
	if _, _, ok := parseDataURL("https://example.com/x.png"); ok {
		t.Fatal("remote URL parsed as data")
	}
}
