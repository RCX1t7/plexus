package harness

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
)

// Emitter is the session-wide event stream adapters write to. It lives as
// long as the session, so events can outlive the turn that caused them.
type Emitter struct {
	ch     chan Event
	done   chan struct{}
	mu     sync.RWMutex
	closed bool
	once   sync.Once
	seq    atomic.Int64
}

// NewEmitter creates a stream.
func NewEmitter() *Emitter {
	return &Emitter{ch: make(chan Event, 64), done: make(chan struct{})}
}

// C is the receive side (Session.Events). Closed by Close.
func (e *Emitter) C() <-chan Event { return e.ch }

// Done is closed when the emitter is closed.
func (e *Emitter) Done() <-chan struct{} { return e.done }

// NextID returns a session-unique id with the given prefix.
func (e *Emitter) NextID(prefix string) string {
	return prefix + "-" + strconv.FormatInt(e.seq.Add(1), 10)
}

// Emit delivers ev (blocking for back-pressure). It returns false once the
// emitter is closed. An empty ev.ID gets a fresh one.
func (e *Emitter) Emit(ev Event) bool {
	if ev.ID == "" {
		ev.ID = e.NextID("ev")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return false
	}
	select {
	case e.ch <- ev:
		return true
	case <-e.done:
		return false
	}
}

// Ask emits a permission event and guarantees reply is called exactly once:
// with the consumer's decision, or deny when ctx ends or the session closes.
func (e *Emitter) Ask(ctx context.Context, ev Event, reply func(Decision)) {
	var once sync.Once
	answered := make(chan struct{})
	ev.Kind = EventPermission
	ev.Decide = func(d Decision) { once.Do(func() { reply(d); close(answered) }) }
	if !e.Emit(ev) {
		ev.Decide(Decision{Reason: "session closed"})
		return
	}
	go func() {
		select {
		case <-answered:
		case <-ctx.Done():
			ev.Decide(Decision{Reason: "turn ended"})
		case <-e.done:
			ev.Decide(Decision{Reason: "session closed"})
		}
	}()
}

// AskQuestions emits a question event; reply gets nil answers on timeout.
func (e *Emitter) AskQuestions(ctx context.Context, ev Event, reply func(Answers)) {
	var once sync.Once
	answered := make(chan struct{})
	ev.Kind = EventQuestion
	ev.Answer = func(a Answers) { once.Do(func() { reply(a); close(answered) }) }
	if !e.Emit(ev) {
		ev.Answer(nil)
		return
	}
	go func() {
		select {
		case <-answered:
		case <-ctx.Done():
			ev.Answer(nil)
		case <-e.done:
			ev.Answer(nil)
		}
	}()
}

// CallTool emits a host tool call; reply gets an error result if nobody
// answers before ctx ends or the session closes.
func (e *Emitter) CallTool(ctx context.Context, ev Event, reply func(HostResult)) {
	var once sync.Once
	answered := make(chan struct{})
	ev.Kind = EventHostTool
	ev.Call = func(r HostResult) { once.Do(func() { reply(r); close(answered) }) }
	if !e.Emit(ev) {
		ev.Call(HostResult{Text: "session closed", IsError: true})
		return
	}
	go func() {
		select {
		case <-answered:
		case <-ctx.Done():
			ev.Call(HostResult{Text: "no active turn", IsError: true})
		case <-e.done:
			ev.Call(HostResult{Text: "session closed", IsError: true})
		}
	}()
}

// Close ends the stream.
func (e *Emitter) Close() {
	e.once.Do(func() {
		close(e.done)
		e.mu.Lock()
		e.closed = true
		close(e.ch)
		e.mu.Unlock()
	})
}

// TurnState tracks the one running turn of a session.
type TurnState struct {
	mu     sync.Mutex
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	seq    atomic.Int64
}

// Begin starts a turn; it fails if one is running.
func (t *TurnState) Begin(parent context.Context) (string, context.Context, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id != "" {
		return "", nil, false
	}
	t.id = "turn-" + strconv.FormatInt(t.seq.Add(1), 10)
	t.ctx, t.cancel = context.WithCancel(parent)
	return t.id, t.ctx, true
}

// Current returns the running turn id and context ("" and Background if none).
func (t *TurnState) Current() (string, context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id == "" {
		return "", context.Background()
	}
	return t.id, t.ctx
}

// End finishes turn id if it is current and reports whether it was.
func (t *TurnState) End(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" || t.id != id {
		return false
	}
	t.cancel()
	t.id = ""
	return true
}
