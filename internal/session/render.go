package session

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/openai"
)

// fingerprint identifies a message by what the model saw, so a client's
// echo of an earlier response matches what the gateway recorded.
func fingerprint(m openai.Message) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(m.Role)
	write(strings.TrimSpace(m.Content.Text()))
	for _, p := range m.Content.Parts {
		if p.ImageURL != nil {
			sum := sha256.Sum256([]byte(p.ImageURL.URL))
			write(hex.EncodeToString(sum[:]))
		}
	}
	for _, tc := range m.ToolCalls {
		write(tc.ID)
		write(tc.Function.Name)
		write(canonicalJSON(tc.Function.Arguments))
	}
	write(m.ToolCallID)
	return hex.EncodeToString(h.Sum(nil))
}

func fingerprints(msgs []openai.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = fingerprint(m)
	}
	return out
}

// canonicalJSON re-encodes a JSON value with sorted keys; invalid input is
// returned trimmed.
func canonicalJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// toolsKey identifies a tool set independent of declaration order.
func toolsKey(tools []openai.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	parts := make([]string, len(tools))
	for i, t := range tools {
		parts[i] = t.Function.Name + "\x00" + t.Function.Description + "\x00" + canonicalJSON(string(t.Function.Parameters))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:])
}

// systemText joins system and developer messages for the runtime's system
// prompt extension.
func systemText(msgs []openai.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if t := strings.TrimSpace(m.Content.Text()); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// trailingStart returns the index where the trailing run of user and tool
// messages begins: the input the model has not answered yet.
func trailingStart(msgs []openai.Message) int {
	i := len(msgs)
	for i > 0 && (msgs[i-1].Role == "user" || msgs[i-1].Role == "tool") {
		i--
	}
	return i
}

// callNames maps tool call ids to function names across the conversation.
func callNames(msgs []openai.Message) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Function.Name
		}
	}
	return names
}

// transcript renders earlier messages as plain text for a fresh runtime
// session. It keeps the newest messages within budget bytes. This replays
// history only; new tool calls still go through the native MCP bridge.
func transcript(msgs []openai.Message, budget int) string {
	names := callNames(msgs)
	var lines []string
	for _, m := range msgs {
		if line := renderLine(m, names); line != "" {
			lines = append(lines, line)
		}
	}
	size := 0
	keep := len(lines)
	for keep > 0 && size+len(lines[keep-1]) <= budget {
		size += len(lines[keep-1]) + 1
		keep--
	}
	out := lines[keep:]
	if keep > 0 {
		out = append([]string{fmt.Sprintf("[%d earlier messages omitted]", keep)}, out...)
	}
	return strings.Join(out, "\n")
}

func renderLine(m openai.Message, names map[string]string) string {
	text := strings.TrimSpace(m.Content.Text())
	switch m.Role {
	case "user":
		images := 0
		for _, p := range m.Content.Parts {
			if p.Type == "image_url" {
				images++
			}
		}
		return strings.TrimSpace("[user]: " + text + strings.Repeat(" [image]", images))
	case "assistant":
		var b strings.Builder
		if text != "" {
			b.WriteString("[assistant]: " + text)
		}
		for _, tc := range m.ToolCalls {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[assistant called tool %s, call id %s, arguments %s]", tc.Function.Name, tc.ID, canonicalJSON(tc.Function.Arguments))
		}
		return b.String()
	case "tool":
		return fmt.Sprintf("[result of tool %s, call id %s]: %s", names[m.ToolCallID], m.ToolCallID, text)
	default:
		return ""
	}
}

// promptBlocks renders new input messages as ACP content blocks.
func promptBlocks(msgs []openai.Message, names map[string]string) []sdk.ContentBlock {
	var blocks []sdk.ContentBlock
	for _, m := range msgs {
		switch m.Role {
		case "user":
			for _, p := range m.Content.Parts {
				switch p.Type {
				case "text":
					if p.Text != "" {
						blocks = append(blocks, sdk.TextBlock(p.Text))
					}
				case "image_url":
					if mime, data, ok := parseDataURL(p.ImageURL.URL); ok {
						blocks = append(blocks, sdk.ImageBlock(data, mime))
					}
				default:
				}
			}
		case "tool":
			blocks = append(blocks, sdk.TextBlock(renderLine(m, names)))
		default:
			if line := renderLine(m, names); line != "" {
				blocks = append(blocks, sdk.TextBlock(line))
			}
		}
	}
	return blocks
}

// parseDataURL splits a base64 data: URL; validation already checked it.
func parseDataURL(u string) (mime, data string, ok bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", false
	}
	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}
	mime, ok = strings.CutSuffix(meta, ";base64")
	if !ok {
		return "", "", false
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "", "", false
	}
	return mime, data, true
}
