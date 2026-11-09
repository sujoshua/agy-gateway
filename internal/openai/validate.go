package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var toolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ToolChoice is the normalized tool_choice.
type ToolChoice string

// Supported tool_choice values.
const (
	ToolChoiceAuto ToolChoice = "auto"
	ToolChoiceNone ToolChoice = "none"
)

// Validate checks a request against what the gateway can honor. When
// ignoreSampling is true, sampling parameters are accepted and ignored
// instead of rejected. It returns the normalized tool_choice.
func Validate(r *ChatRequest, ignoreSampling bool) (ToolChoice, *Error) {
	if strings.TrimSpace(r.Model) == "" {
		return "", InvalidRequest("model", "model is required")
	}
	if err := validateUnsupported(r, ignoreSampling); err != nil {
		return "", err
	}
	if r.StreamOptions != nil && !r.Stream {
		return "", InvalidRequest("stream_options", "stream_options is only allowed when stream is true")
	}
	switch r.ReasoningEffort {
	case "", "low", "medium", "high":
	default:
		return "", Unsupported("reasoning_effort", "reasoning_effort %q is not supported; use low, medium or high", r.ReasoningEffort)
	}
	if err := validateTools(r.Tools); err != nil {
		return "", err
	}
	choice, err := parseToolChoice(r.ToolChoice)
	if err != nil {
		return "", err
	}
	if err := validateMessages(r.Messages); err != nil {
		return "", err
	}
	return choice, nil
}

func validateUnsupported(r *ChatRequest, ignoreSampling bool) *Error {
	if r.N != nil && *r.N != 1 {
		return Unsupported("n", "only n=1 is supported")
	}
	if r.Logprobs != nil && *r.Logprobs {
		return Unsupported("logprobs", "logprobs are not available from the runtime")
	}
	if r.TopLogprobs != nil {
		return Unsupported("top_logprobs", "logprobs are not available from the runtime")
	}
	if len(r.LogitBias) > 0 {
		return Unsupported("logit_bias", "logit_bias is not supported")
	}
	if r.ResponseFormat != nil && r.ResponseFormat.Type != "text" && r.ResponseFormat.Type != "json_object" {
		return Unsupported("response_format", "response_format %q is not supported; use text or json_object", r.ResponseFormat.Type)
	}
	for _, m := range r.Modalities {
		if m != "text" {
			return Unsupported("modalities", "only the text modality is supported")
		}
	}
	for name, raw := range map[string]json.RawMessage{
		"audio":              r.Audio,
		"prediction":         r.Prediction,
		"web_search_options": r.WebSearchOptions,
	} {
		if present(raw) {
			return Unsupported(name, "%s is not supported", name)
		}
	}
	if present(r.Functions) || present(r.FunctionCall) {
		return Unsupported("functions", "legacy functions/function_call are not supported; use tools and tool_choice")
	}
	if ignoreSampling {
		return nil
	}
	sampling := []struct {
		name string
		set  bool
	}{
		{"temperature", r.Temperature != nil},
		{"top_p", r.TopP != nil},
		{"presence_penalty", r.PresencePenalty != nil},
		{"frequency_penalty", r.FrequencyPenalty != nil},
		{"seed", r.Seed != nil},
		{"stop", present(r.Stop)},
		{"max_tokens", r.MaxTokens != nil},
		{"max_completion_tokens", r.MaxCompletionTokens != nil},
	}
	for _, s := range sampling {
		if s.set {
			return Unsupported(s.name, "%s cannot be controlled through the Antigravity runtime; omit it or set AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true", s.name)
		}
	}
	return nil
}

func present(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

func validateTools(tools []Tool) *Error {
	seen := map[string]bool{}
	for i, t := range tools {
		param := fmt.Sprintf("tools[%d]", i)
		if t.Type != "function" {
			return Unsupported(param+".type", "tool type %q is not supported; only function tools are", t.Type)
		}
		if !toolNameRE.MatchString(t.Function.Name) {
			return InvalidRequest(param+".function.name", "tool name %q must match ^[a-zA-Z0-9_-]{1,64}$", t.Function.Name)
		}
		if seen[t.Function.Name] {
			return InvalidRequest(param+".function.name", "duplicate tool name %q", t.Function.Name)
		}
		seen[t.Function.Name] = true
		if present(t.Function.Parameters) {
			var obj map[string]any
			if err := json.Unmarshal(t.Function.Parameters, &obj); err != nil {
				return InvalidRequest(param+".function.parameters", "parameters must be a JSON Schema object")
			}
		}
	}
	return nil
}

func parseToolChoice(raw json.RawMessage) (ToolChoice, *Error) {
	if !present(raw) {
		return ToolChoiceAuto, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return ToolChoiceAuto, nil
		case "none":
			return ToolChoiceNone, nil
		case "required":
			return "", Unsupported("tool_choice", `tool_choice "required" cannot be enforced through the runtime; use "auto"`)
		default:
			return "", InvalidRequest("tool_choice", "invalid tool_choice %q", s)
		}
	}
	return "", Unsupported("tool_choice", `forcing a specific function cannot be enforced through the runtime; use "auto"`)
}

func validateMessages(msgs []Message) *Error {
	if len(msgs) == 0 {
		return InvalidRequest("messages", "messages must not be empty")
	}
	callIDs := map[string]bool{}
	answered := map[string]bool{}
	for i, m := range msgs {
		param := fmt.Sprintf("messages[%d]", i)
		switch m.Role {
		case "system", "developer":
			for _, p := range m.Content.Parts {
				if p.Type != "text" {
					return Unsupported(param+".content", "%s messages may only contain text", m.Role)
				}
			}
		case "user":
			if !m.Content.Set {
				return InvalidRequest(param+".content", "user content is required")
			}
			for j, p := range m.Content.Parts {
				if err := validateUserPart(p, fmt.Sprintf("%s.content[%d]", param, j)); err != nil {
					return err
				}
			}
		case "assistant":
			for j, tc := range m.ToolCalls {
				tp := fmt.Sprintf("%s.tool_calls[%d]", param, j)
				if tc.ID == "" {
					return InvalidRequest(tp+".id", "tool call id is required")
				}
				if tc.Type != "" && tc.Type != "function" {
					return Unsupported(tp+".type", "tool call type %q is not supported", tc.Type)
				}
				if tc.Function.Name == "" {
					return InvalidRequest(tp+".function.name", "tool call function name is required")
				}
				if a := strings.TrimSpace(tc.Function.Arguments); a != "" && !json.Valid([]byte(a)) {
					return InvalidRequest(tp+".function.arguments", "tool call arguments must be valid JSON")
				}
				callIDs[tc.ID] = true
			}
			for _, p := range m.Content.Parts {
				if p.Type != "text" && p.Type != "refusal" {
					return Unsupported(param+".content", "assistant messages may only contain text")
				}
			}
		case "tool":
			if m.ToolCallID == "" {
				return InvalidRequest(param+".tool_call_id", "tool messages require tool_call_id")
			}
			if !callIDs[m.ToolCallID] {
				return InvalidRequest(param+".tool_call_id", "tool_call_id %q does not match any earlier assistant tool call", m.ToolCallID)
			}
			if answered[m.ToolCallID] {
				return InvalidRequest(param+".tool_call_id", "duplicate result for tool_call_id %q", m.ToolCallID)
			}
			answered[m.ToolCallID] = true
			for _, p := range m.Content.Parts {
				if p.Type != "text" {
					return Unsupported(param+".content", "tool results may only contain text")
				}
			}
		case "function":
			return Unsupported(param+".role", "the legacy function role is not supported; use tool")
		default:
			return InvalidRequest(param+".role", "unknown role %q", m.Role)
		}
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" && last.Role != "tool" {
		return Unsupported("messages", "the last message must be from user or tool; assistant prefill is not supported")
	}
	return nil
}

func validateUserPart(p Part, param string) *Error {
	switch p.Type {
	case "text":
		return nil
	case "image_url":
		if p.ImageURL == nil || !strings.HasPrefix(p.ImageURL.URL, "data:image/") || !strings.Contains(p.ImageURL.URL, ";base64,") {
			return Unsupported(param+".image_url", "only base64 data: image URLs are supported; the gateway does not fetch remote images")
		}
		return nil
	default:
		return Unsupported(param+".type", "content part type %q is not supported", p.Type)
	}
}
