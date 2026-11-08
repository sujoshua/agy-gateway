// Package openai holds the OpenAI Chat Completions wire types the gateway
// accepts and produces, plus request validation.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ChatRequest is a POST /v1/chat/completions body. Fields the gateway cannot
// honor are decoded so validation can reject them explicitly.
type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	// User scopes conversation reuse to one end user when set.
	User string `json:"user,omitempty"`

	// Semantic parameters the runtime offers no control over.
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	LogitBias           map[string]any  `json:"logit_bias,omitempty"`
	Logprobs            *bool           `json:"logprobs,omitempty"`
	TopLogprobs         *int            `json:"top_logprobs,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	Modalities          []string        `json:"modalities,omitempty"`
	Audio               json.RawMessage `json:"audio,omitempty"`
	Prediction          json.RawMessage `json:"prediction,omitempty"`
	WebSearchOptions    json.RawMessage `json:"web_search_options,omitempty"`
	Functions           json.RawMessage `json:"functions,omitempty"`
	FunctionCall        json.RawMessage `json:"function_call,omitempty"`
}

// StreamOptions mirrors stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ResponseFormat mirrors response_format; only "text" is supported.
type ResponseFormat struct {
	Type string `json:"type"`
}

// Tool is a function tool definition.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Message is one chat message in either direction.
type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Refusal    *string    `json:"refusal,omitempty"`
	// ReasoningContent carries runtime thoughts on responses. It is accepted
	// and ignored on requests.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall is an assistant function call. Index is set only in stream deltas.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names a function and its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// Content is message content: null, a string, or an array of parts.
type Content struct {
	Parts []Part
	// Set reports whether the field held a string or array (not null).
	Set bool
}

// Part is one content part.
type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is an image_url content part payload.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// TextContent builds string content.
func TextContent(s string) Content {
	return Content{Parts: []Part{{Type: "text", Text: s}}, Set: true}
}

// Text concatenates the text parts.
func (c Content) Text() string {
	var b strings.Builder
	for i, p := range c.Parts {
		if p.Type == "text" {
			if i > 0 && b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// UnmarshalJSON accepts null, a string, or an array of parts.
func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*c = Content{}
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = TextContent(s)
		return nil
	case len(b) > 0 && b[0] == '[':
		var parts []Part
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		*c = Content{Parts: parts, Set: true}
		return nil
	default:
		return errors.New("content must be a string, an array of parts, or null")
	}
}

// MarshalJSON writes a plain string when the content is text only.
func (c Content) MarshalJSON() ([]byte, error) {
	if !c.Set {
		return []byte("null"), nil
	}
	textOnly := true
	for _, p := range c.Parts {
		if p.Type != "text" {
			textOnly = false
		}
	}
	if textOnly {
		return json.Marshal(c.Text())
	}
	return json.Marshal(c.Parts)
}

// ChatCompletion is a non-streaming response.
type ChatCompletion struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint *string  `json:"system_fingerprint"`
}

// Choice is one non-streaming choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Logprobs     any     `json:"logprobs"`
}

// Chunk is one streaming chunk.
type Chunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	Choices           []ChunkChoice `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
	SystemFingerprint *string       `json:"system_fingerprint"`
}

// ChunkChoice is one streaming choice.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
	Logprobs     any     `json:"logprobs"`
}

// Delta is a streaming message fragment.
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          *string    `json:"content,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Usage reports token counts exactly as the runtime returned them.
type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails splits prompt tokens.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CompletionTokensDetails splits completion tokens.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Model is one /v1/models entry.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the /v1/models response.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
