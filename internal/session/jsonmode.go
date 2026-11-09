package session

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/bridge"
	"github.com/sujoshua/agy-gateway/internal/openai"
)

// The runtime offers no structured-output setting, so json_object is a
// prompt instruction plus a check of the reply. It is best effort: the
// gateway retries once and then fails rather than pass on invalid JSON.
const (
	jsonInstruction = "Reply with exactly one JSON object and nothing else: no prose before or after it and no Markdown code fence."
	jsonRetry       = "Your previous reply was not a single valid JSON object. Reply again with only the JSON object, no prose and no Markdown code fence."
)

// jsonEmitter holds back text until the reply can be checked. Reasoning
// and tool calls pass through; text that precedes a tool call is released
// unchecked, since only a final answer must be JSON.
type jsonEmitter struct {
	next Emitter
	buf  strings.Builder
	open bool // text passes through after a tool call
	sent bool // something reached the client
}

func (j *jsonEmitter) Text(s string) error {
	if j.open {
		j.sent = true
		return j.next.Text(s)
	}
	j.buf.WriteString(s)
	return nil
}

func (j *jsonEmitter) Reasoning(s string) error {
	j.sent = true
	return j.next.Reasoning(s)
}

func (j *jsonEmitter) ToolCall(index int, c *bridge.Call) error {
	if err := j.release(j.buf.String()); err != nil {
		return err
	}
	j.open = true
	j.sent = true
	return j.next.ToolCall(index, c)
}

// release sends held text and empties the buffer.
func (j *jsonEmitter) release(s string) error {
	j.buf.Reset()
	if s == "" {
		return nil
	}
	j.sent = true
	return j.next.Text(s)
}

// finishJSON checks a final answer, asking the model once more when it is
// not a JSON object. Replies cut short or ending in tool calls are passed
// on as they are.
func (m *Manager) finishJSON(ctx context.Context, s *Session, rq *Request, j *jsonEmitter, out *Outcome) (*Outcome, *Partial) {
	for retried := false; ; retried = true {
		if out.FinishReason != "stop" || len(out.ToolCalls) > 0 {
			if err := j.release(j.buf.String()); err != nil {
				return nil, &Partial{Err: openai.Internal("writing the response failed"), Sent: true}
			}
			return out, nil
		}
		if obj, ok := jsonObject(out.Content); ok {
			out.Content = obj
			if err := j.release(obj); err != nil {
				return nil, &Partial{Err: openai.Internal("writing the response failed"), Sent: true}
			}
			return out, nil
		}
		if retried {
			return nil, &Partial{Err: openai.Upstream("the model did not reply with a valid JSON object"), Sent: j.sent}
		}
		m.log.Info("reply is not a JSON object; asking again", logSession(s.id))
		j.buf.Reset()
		first := out.Usage
		s.startTurn(m.ctx, []sdk.ContentBlock{sdk.TextBlock(jsonRetry)})
		var p *Partial
		if out, p = m.consume(ctx, s, rq, j); p != nil {
			p.Sent = j.sent
			return nil, p
		}
		out.Usage = addUsage(first, out.Usage)
	}
}

// jsonObject returns s as a JSON object without surrounding space or a
// Markdown code fence.
func jsonObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if body, ok := strings.CutPrefix(s, "```"); ok {
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			if body, ok = strings.CutSuffix(body[nl+1:], "```"); ok {
				s = strings.TrimSpace(body)
			}
		}
	}
	// The prefix check rules out null, which decodes into a map.
	var obj map[string]json.RawMessage
	if !strings.HasPrefix(s, "{") || json.Unmarshal([]byte(s), &obj) != nil {
		return "", false
	}
	return s, true
}

// addUsage sums the counts of two turns. It returns nil when either turn
// reported none, since counts are never estimated.
func addUsage(a, b *openai.Usage) *openai.Usage {
	if a == nil || b == nil {
		return nil
	}
	sum := &openai.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
	if a.PromptTokensDetails != nil && b.PromptTokensDetails != nil {
		sum.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: a.PromptTokensDetails.CachedTokens + b.PromptTokensDetails.CachedTokens}
	}
	if a.CompletionTokensDetails != nil && b.CompletionTokensDetails != nil {
		sum.CompletionTokensDetails = &openai.CompletionTokensDetails{ReasoningTokens: a.CompletionTokensDetails.ReasoningTokens + b.CompletionTokensDetails.ReasoningTokens}
	}
	return sum
}
