package openai

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, s string) *ChatRequest {
	t.Helper()
	var r ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return &r
}

func TestValidateAccepts(t *testing.T) {
	for _, s := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"developer","content":[{"type":"text","text":"d"}]},{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`,
		`{"model":"m","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[
			{"role":"user","content":"w"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"P\"}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"sunny"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1,"response_format":{"type":"text"},"reasoning_effort":"low","user":"u","metadata":{"a":"b"}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
	} {
		if _, err := Validate(decode(t, s), false); err != nil {
			t.Errorf("%s: unexpected error %v", s, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"no model":        `{"messages":[{"role":"user","content":"hi"}]}`,
		"empty":           `{"model":"m","messages":[]}`,
		"temperature":     `{"model":"m","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`,
		"max_tokens":      `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		"n":               `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`,
		"json schema":     `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"x"}},"messages":[{"role":"user","content":"hi"}]}`,
		"required":        `{"model":"m","tool_choice":"required","messages":[{"role":"user","content":"hi"}]}`,
		"named":           `{"model":"m","tool_choice":{"type":"function","function":{"name":"x"}},"messages":[{"role":"user","content":"hi"}]}`,
		"orphan result":   `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"x","content":"r"}]}`,
		"dup result":      `{"model":"m","messages":[{"role":"user","content":"w"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"a"},{"role":"tool","tool_call_id":"c","content":"b"}]}`,
		"bad args":        `{"model":"m","messages":[{"role":"user","content":"w"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{"}}]},{"role":"tool","tool_call_id":"c","content":"a"}]}`,
		"prefill":         `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x"}]}`,
		"remote image":    `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		"bad tool name":   `{"model":"m","tools":[{"type":"function","function":{"name":"a b"}}],"messages":[{"role":"user","content":"hi"}]}`,
		"dup tool":        `{"model":"m","tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"a"}}],"messages":[{"role":"user","content":"hi"}]}`,
		"opts no stream":  `{"model":"m","stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
		"function role":   `{"model":"m","messages":[{"role":"function","name":"f","content":"x"},{"role":"user","content":"hi"}]}`,
		"legacy function": `{"model":"m","functions":[{"name":"f"}],"messages":[{"role":"user","content":"hi"}]}`,
		"logprobs":        `{"model":"m","logprobs":true,"messages":[{"role":"user","content":"hi"}]}`,
		"effort":          `{"model":"m","reasoning_effort":"minimal","messages":[{"role":"user","content":"hi"}]}`,
	}
	for name, s := range cases {
		if _, err := Validate(decode(t, s), false); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestIgnoreSampling(t *testing.T) {
	r := decode(t, `{"model":"m","temperature":0.2,"max_tokens":5,"stop":["x"],"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := Validate(r, true); err != nil {
		t.Fatalf("expected sampling params to be ignored: %v", err)
	}
}

func TestContentRoundTrip(t *testing.T) {
	var c Content
	if err := json.Unmarshal([]byte(`null`), &c); err != nil || c.Set {
		t.Fatalf("null: %v %+v", err, c)
	}
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`), &c); err != nil || c.Text() != "a\nb" {
		t.Fatalf("parts: %v %q", err, c.Text())
	}
	b, err := json.Marshal(TextContent("x"))
	if err != nil || string(b) != `"x"` {
		t.Fatalf("marshal: %s %v", b, err)
	}
	if err := json.Unmarshal([]byte(`5`), &c); err == nil {
		t.Fatal("number content should fail")
	}
}
