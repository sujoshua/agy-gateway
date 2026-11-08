//go:build integration

// Package integration drives the gateway with the official OpenAI Go SDK
// against a real, already signed-in ACP runtime. It never signs in or out.
//
//	AGY_GW_RUNTIME_CMD=/path/to/agy_acp_server.par make test-integration
package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/sujoshua/agy-gateway/internal/app"
	"github.com/sujoshua/agy-gateway/internal/config"
)

const apiKey = "integration-key"

type gateway struct {
	app    *app.App
	client openai.Client
	model  string
}

func start(t *testing.T) *gateway {
	t.Helper()
	if os.Getenv("AGY_GW_RUNTIME_CMD") == "" {
		t.Skip("set AGY_GW_RUNTIME_CMD to the runtime binary to run integration tests")
	}
	cfg, err := config.Load(func(k string) string {
		switch k {
		case "AGY_GW_API_KEYS":
			return apiKey
		case "AGY_GW_WORKDIR":
			return t.TempDir()
		case "AGY_GW_BRIDGE_LISTEN":
			return "127.0.0.1:0"
		}
		return os.Getenv(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	level := slog.LevelWarn
	if testing.Verbose() {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, cfg, log, "integration", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Run(ctx) }()
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
	})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if ok, _ := a.Runtime.Ready(); ok {
			break
		}
		if time.Now().After(deadline) {
			_, status := a.Runtime.Ready()
			t.Fatalf("runtime not ready: %s", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	model := os.Getenv("AGY_GW_TEST_MODEL")
	if model == "" {
		model = a.Runtime.Models()[0].ID + "-low"
	}
	client := openai.NewClient(option.WithBaseURL(srv.URL+"/v1"), option.WithAPIKey(apiKey), option.WithMaxRetries(0))
	return &gateway{app: a, client: client, model: model}
}

var weatherTool = openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
	Name:        "get_weather",
	Description: openai.String("Get the current weather for a city."),
	Parameters: openai.FunctionParameters{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string", "description": "City name"}},
		"required":   []string{"city"},
	},
})

var weather = map[string]string{
	"paris": `{"temp_c":17,"condition":"light rain"}`,
	"tokyo": `{"temp_c":24,"condition":"clear"}`,
}

// answer runs a tool call the way a client would.
func answer(t *testing.T, call openai.ChatCompletionMessageToolCallUnion) (city, result string) {
	t.Helper()
	if call.Function.Name != "get_weather" {
		t.Fatalf("unexpected tool %q", call.Function.Name)
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments %q: %v", call.Function.Arguments, err)
	}
	city = strings.ToLower(strings.TrimSpace(args.City))
	if r, ok := weather[city]; ok {
		return city, r
	}
	return city, `{"error":"unknown city"}`
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestModels(t *testing.T) {
	g := start(t)
	page, err := g.client.Models.List(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) == 0 {
		t.Fatal("no models")
	}
	for _, m := range page.Data {
		t.Logf("model %s", m.ID)
	}
}

// TestGetWeatherToolLoop is the acceptance test: a native tool call, its
// result, the final answer, and a follow-up turn on the same session.
func TestGetWeatherToolLoop(t *testing.T) {
	g := start(t)
	ctx := ctxT(t)
	params := openai.ChatCompletionNewParams{
		Model: g.model,
		Tools: []openai.ChatCompletionToolUnionParam{weatherTool},
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are a concise assistant. Use tools for facts."),
			openai.UserMessage("What is the weather in Paris right now? Use the get_weather tool."),
		},
	}
	first, err := g.client.Chat.Completions.New(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	choice := first.Choices[0]
	if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
		t.Fatalf("want a tool call, got finish %q: %q", choice.FinishReason, choice.Message.Content)
	}
	params.Messages = append(params.Messages, choice.Message.ToParam())
	for _, call := range choice.Message.ToolCalls {
		city, result := answer(t, call)
		if city != "paris" {
			t.Fatalf("want city paris, got %q", city)
		}
		params.Messages = append(params.Messages, openai.ToolMessage(result, call.ID))
	}
	final, err := g.client.Chat.Completions.New(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	text := final.Choices[0].Message.Content
	t.Logf("final: %s", text)
	if final.Choices[0].FinishReason != "stop" || !strings.Contains(strings.ToLower(text), "17") && !strings.Contains(strings.ToLower(text), "rain") {
		t.Fatalf("final answer does not use the tool result: finish %q, %q", final.Choices[0].FinishReason, text)
	}
	if final.Usage.PromptTokens == 0 || final.Usage.TotalTokens == 0 {
		t.Fatalf("missing usage: %+v", final.Usage)
	}

	// Follow-up turn: the gateway must reuse the runtime session.
	params.Messages = append(params.Messages, final.Choices[0].Message.ToParam(),
		openai.UserMessage("Repeat the temperature you just told me, as a bare number."))
	follow, err := g.client.Chat.Completions.New(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if got := follow.Choices[0].Message.Content; !strings.Contains(got, "17") {
		t.Fatalf("follow-up lost context: %q", got)
	}
	if n := g.app.Sessions.Len(); n != 1 {
		t.Fatalf("want 1 runtime session, have %d", n)
	}
}

// TestStreamingParallelTools streams a request that needs two calls and
// answers every call until the model stops.
func TestStreamingParallelTools(t *testing.T) {
	g := start(t)
	ctx := ctxT(t)
	params := openai.ChatCompletionNewParams{
		Model:         g.model,
		Tools:         []openai.ChatCompletionToolUnionParam{weatherTool},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Compare the weather in Paris and Tokyo. Call get_weather for both cities, in parallel if you can."),
		},
	}
	seen := map[string]bool{}
	for round := 0; ; round++ {
		if round > 6 {
			t.Fatal("too many rounds")
		}
		stream := g.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}
		chunks := 0
		for stream.Next() {
			acc.AddChunk(stream.Current())
			chunks++
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		choice := acc.Choices[0]
		t.Logf("round %d: %d chunks, finish %q, %d tool calls", round, chunks, choice.FinishReason, len(choice.Message.ToolCalls))
		if choice.FinishReason != "tool_calls" {
			if !seen["paris"] || !seen["tokyo"] {
				t.Fatalf("model stopped before calling both cities: %v", seen)
			}
			if acc.Usage.TotalTokens == 0 {
				t.Fatal("final stream carried no usage")
			}
			t.Logf("final: %s", choice.Message.Content)
			return
		}
		params.Messages = append(params.Messages, choice.Message.ToParam())
		for _, call := range choice.Message.ToolCalls {
			city, result := answer(t, call)
			seen[city] = true
			params.Messages = append(params.Messages, openai.ToolMessage(result, call.ID))
		}
	}
}

// TestStreamHi is the minimal smoke test many clients run first: a streamed "hi".
func TestStreamHi(t *testing.T) {
	g := start(t)
	stream := g.client.Chat.Completions.NewStreaming(ctxT(t), openai.ChatCompletionNewParams{
		Model:    g.model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if acc.Choices[0].Message.Content == "" {
		t.Fatal("empty reply")
	}
}
