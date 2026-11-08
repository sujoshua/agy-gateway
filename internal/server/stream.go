package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/sujoshua/agy-gateway/internal/bridge"
	"github.com/sujoshua/agy-gateway/internal/openai"
	"github.com/sujoshua/agy-gateway/internal/session"
)

// argChunk is the size of streamed tool argument fragments.
const argChunk = 48

// stream writes a chat completion as server-sent events. Headers are sent
// lazily, so an error before any output can still be a plain HTTP error
// that a router may fail over on.
type stream struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	id      string
	model   string
	created int64

	mu      sync.Mutex
	started bool
	last    time.Time
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func newStream(w http.ResponseWriter, id, model string, created int64, keepalive time.Duration) *stream {
	st := &stream{w: w, rc: http.NewResponseController(w), id: id, model: model, created: created, last: time.Now(), done: make(chan struct{})}
	st.wg.Go(func() { st.keepalive(keepalive) })
	return st
}

// keepalive commits the response and sends SSE comments while the runtime
// is silent (long thinking without reasoning output).
func (st *stream) keepalive(every time.Duration) {
	t := time.NewTicker(every / 3)
	defer t.Stop()
	for {
		select {
		case <-st.done:
			return
		case <-t.C:
			st.mu.Lock()
			if time.Since(st.last) >= every {
				st.begin()
				_, _ = st.w.Write([]byte(": keepalive\n\n"))
				_ = st.rc.Flush()
				st.last = time.Now()
			}
			st.mu.Unlock()
		}
	}
}

// stop ends the keepalive goroutine and waits for it, so nothing writes
// after the handler returns. Callers must not hold mu.
func (st *stream) stop() {
	st.once.Do(func() { close(st.done) })
	st.wg.Wait()
}

func (st *stream) committed() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.started
}

// begin sends headers and the role chunk; callers hold mu.
func (st *stream) begin() {
	if st.started {
		return
	}
	st.started = true
	h := st.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	st.w.WriteHeader(http.StatusOK)
	_ = st.writeLocked(openai.Delta{Role: "assistant", Content: new(string)}, nil, nil)
}

func (st *stream) send(d openai.Delta, finish *string, usage *openai.Usage) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.begin()
	return st.writeLocked(d, finish, usage)
}

func (st *stream) writeLocked(d openai.Delta, finish *string, usage *openai.Usage) error {
	c := openai.Chunk{
		ID: st.id, Object: "chat.completion.chunk", Created: st.created, Model: st.model,
		Choices: []openai.ChunkChoice{{Index: 0, Delta: d, FinishReason: finish}},
	}
	if usage != nil {
		c.Choices = []openai.ChunkChoice{}
		c.Usage = usage
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return st.raw("data: " + string(b) + "\n\n")
}

func (st *stream) raw(s string) error {
	st.last = time.Now()
	if _, err := st.w.Write([]byte(s)); err != nil {
		return err
	}
	return st.rc.Flush()
}

func (st *stream) Text(s string) error { return st.send(openai.Delta{Content: &s}, nil, nil) }

func (st *stream) Reasoning(s string) error {
	return st.send(openai.Delta{ReasoningContent: &s}, nil, nil)
}

// ToolCall streams the call header, then its arguments in fragments, as
// OpenAI does.
func (st *stream) ToolCall(index int, c *bridge.Call) error {
	i := index
	head := openai.ToolCall{Index: &i, ID: c.ID, Type: "function", Function: openai.FunctionCall{Name: c.Name}}
	if err := st.send(openai.Delta{ToolCalls: []openai.ToolCall{head}}, nil, nil); err != nil {
		return err
	}
	args := c.Args
	for args != "" {
		n := min(argChunk, len(args))
		part := openai.ToolCall{Index: &i, Function: openai.FunctionCall{Arguments: args[:n]}}
		if err := st.send(openai.Delta{ToolCalls: []openai.ToolCall{part}}, nil, nil); err != nil {
			return err
		}
		args = args[n:]
	}
	return nil
}

func (st *stream) finish(out *session.Outcome, includeUsage bool) {
	st.stop()
	reason := out.FinishReason
	_ = st.send(openai.Delta{}, &reason, nil)
	st.mu.Lock()
	defer st.mu.Unlock()
	if includeUsage && out.Usage != nil {
		_ = st.writeLocked(openai.Delta{}, nil, out.Usage)
	}
	_ = st.raw("data: [DONE]\n\n")
}

// fail ends a stream that already sent output. It sends an error event and
// no [DONE], so clients see the response as failed.
func (st *stream) fail(e *openai.Error) {
	st.stop()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.begin()
	_ = st.raw("data: " + string(e.MarshalEnvelope()) + "\n\n")
}

// collector gathers a non-streaming response; the Outcome carries it all.
type collector struct{}

func (*collector) Text(string) error                { return nil }
func (*collector) Reasoning(string) error           { return nil }
func (*collector) ToolCall(int, *bridge.Call) error { return nil }
