package session

import (
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/acp"
	"github.com/sujoshua/agy-gateway/internal/bridge"
)

type eventKind int

const (
	evText eventKind = iota
	evThought
	evToolCall
	evQuota // the runtime's report of an exhausted quota
	evDone
)

// event is one thing that happened during a runtime turn.
type event struct {
	kind  eventKind
	text  string
	call  *bridge.Call
	stop  sdk.StopReason
	usage *sdk.Usage
	err   error
}

// queue is an unbounded FIFO of turn events. Producers never block, so a
// slow or absent HTTP reader cannot stall the ACP connection.
type queue struct {
	mu    sync.Mutex
	items []event
	wake  chan struct{}
}

func newQueue() *queue { return &queue{wake: make(chan struct{}, 1)} }

func (q *queue) push(e event) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) pop() (event, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return event{}, false
	}
	e := q.items[0]
	q.items[0] = event{}
	q.items = q.items[1:]
	return e, true
}

func (q *queue) reset() {
	q.mu.Lock()
	q.items = nil
	q.mu.Unlock()
}

// turn is one session/prompt call; done closes after its Done event.
type turn struct{ done chan struct{} }

func (t *turn) active() bool {
	if t == nil {
		return false
	}
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// Session is one runtime conversation bound to one OpenAI conversation.
type Session struct {
	m        *Manager
	id       string // ACP session id
	owner    string
	toolsKey string
	workDir  string
	tenant   *bridge.Tenant
	q        *queue

	// Fields below are guarded by m.mu.
	busy        bool
	closed      bool
	needsResume bool
	lastUsed    time.Time
	history     []string // message fingerprints the runtime has seen
	model       acp.Model

	mu sync.Mutex
	// Fields below are guarded by mu.
	cur      *turn
	emitted  map[string]*bridge.Call // calls returned to the client, awaiting results
	answered map[string]bool         // call ids whose results were consumed
	expired  map[string]*bridge.Call // calls that settled without a client result
}

func (s *Session) currentTurn() *turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// ToolCall implements bridge.Sink.
func (s *Session) ToolCall(c *bridge.Call) { s.q.push(event{kind: evToolCall, call: c}) }

// CallExpired implements bridge.Sink.
func (s *Session) CallExpired(c *bridge.Call, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answered[c.ID] {
		return
	}
	delete(s.emitted, c.ID)
	s.expired[c.ID] = c
	s.m.log.Info("tool call settled without a client result", logSession(s.id), logCall(c.ID), logReason(reason))
}

func (s *Session) onUpdate(n sdk.SessionNotification) {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil && t.Text != "" {
			kind := evText
			if isQuotaMessage(t.Text) {
				kind = evQuota
			}
			s.q.push(event{kind: kind, text: t.Text})
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content.Text; t != nil && t.Text != "" {
			s.q.push(event{kind: evThought, text: t.Text})
		}
	case u.ToolCall != nil:
		// Calls into the bridge surface through MCP; the ACP notification is
		// only bookkeeping and is hidden from the client.
		if !acp.IsOwnMCPCall(u.ToolCall.Meta, s.m.opt.MCPServerName) {
			s.m.log.Debug("runtime tool call (not exposed)", logSession(s.id), logTitle(u.ToolCall.Title))
		}
	default:
	}
}
