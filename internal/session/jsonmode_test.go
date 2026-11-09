package session

import "testing"

func TestJSONObject(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                 `{"a":1}`,
		"  {\"a\": [1, 2]}\n":     `{"a": [1, 2]}`,
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```\n":   `{"a":1}`,
		"[1, 2]":                  "",
		"null":                    "",
		`{"a":1} {"b":2}`:         "",
		"Here it is: {\"a\":1}":   "",
		"```json\n{\"a\":1}":      "",
		`{"a":`:                   "",
	}
	for in, want := range cases {
		got, ok := jsonObject(in)
		if got != want || ok != (want != "") {
			t.Errorf("jsonObject(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
