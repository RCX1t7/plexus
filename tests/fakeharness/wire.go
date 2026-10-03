package fakeharness

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Driver is one native protocol on stdio.
type Driver interface {
	// Serve reads hub messages until EOF. It returns when stdin closes.
	Serve(in io.Reader) error
}

// NewDriver picks the protocol for proto (see DetectProto).
func NewDriver(proto string, b *Brain, out io.Writer, opts DriverOptions) (Driver, error) {
	w := &lineWriter{w: out}
	switch proto {
	case ProtoClaude:
		return newClaude(b, w, opts), nil
	case ProtoCodex:
		return newCodex(b, w), nil
	case ProtoDSH:
		return newDSH(b, w), nil
	case ProtoACP:
		return newACP(b, w), nil
	}
	return nil, errors.New("unknown protocol " + proto)
}

// DriverOptions carries launch flags that matter to a protocol.
type DriverOptions struct {
	ResumeID string // claude --resume=<id>
	// Bypass: launched in a mode without a blocking permission callback
	// (--dangerously-skip-permissions / --permission-mode bypassPermissions):
	// tools run without asking the hub. Plexus must refuse to start such a
	// partner unless bots[].ungated_ok (ARCHITECTURE §7.1).
	Bypass bool
}

const (
	ProtoClaude = "claude-stream-json"
	ProtoCodex  = "codex-app-server"
	ProtoDSH    = "dsh-plexus-bridge"
	ProtoACP    = "acp"
)

// DetectProto maps the launch argv (as Plexus builds it) to a protocol.
func DetectProto(args []string) string {
	for i, a := range args {
		switch {
		case a == "app-server" && i == 0:
			return ProtoCodex
		case a == "--input-format" && i+1 < len(args) && args[i+1] == "stream-json":
			return ProtoClaude
		case a == "--input-format=stream-json":
			return ProtoClaude
		case a == "--profile" && i+1 < len(args) && args[i+1] == "plexus":
			return ProtoDSH
		case a == "--profile=plexus":
			return ProtoDSH
		case a == "--acp" || a == "--experimental-acp" || (a == "--profile" && i+1 < len(args) && args[i+1] == "acp"):
			return ProtoACP
		}
	}
	return ""
}

// ------------------------------------------------------------------ line I/O

type lineWriter struct {
	mu   sync.Mutex
	w    io.Writer
	gone atomic.Bool
}

func (l *lineWriter) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gone.Load() {
		return
	}
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		l.gone.Store(true) // EPIPE: the hub died
	}
}

func scanLines(in io.Reader, fn func([]byte)) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := append([]byte(nil), line...)
		fn(cp)
	}
}

// ------------------------------------------------------------------ JSON-RPC peer

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// rpcPeer is a symmetric JSON-RPC endpoint over lines. withVersion adds
// "jsonrpc":"2.0" (ACP, DSH bridge); Codex app-server omits it.
type rpcPeer struct {
	w           *lineWriter
	withVersion bool
	seq         atomic.Int64
	mu          sync.Mutex
	pending     map[string]chan rpcMsg
	closed      chan struct{}
	closeOnce   sync.Once
	// onRequest must call reply exactly once (may be asynchronous).
	onRequest func(method string, params json.RawMessage, reply func(result any, err *RPCError))
	onNotify  func(method string, params json.RawMessage)
}

func newPeer(w *lineWriter, withVersion bool) *rpcPeer {
	return &rpcPeer{w: w, withVersion: withVersion, pending: map[string]chan rpcMsg{}, closed: make(chan struct{})}
}

func (p *rpcPeer) ver() string {
	if p.withVersion {
		return "2.0"
	}
	return ""
}

func (p *rpcPeer) serve(in io.Reader) {
	scanLines(in, func(line []byte) {
		var m rpcMsg
		if json.Unmarshal(line, &m) != nil {
			return
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			id := m.ID
			var once sync.Once
			p.onRequest(m.Method, m.Params, func(result any, err *RPCError) {
				once.Do(func() {
					out := map[string]any{"id": id}
					if p.withVersion {
						out["jsonrpc"] = "2.0"
					}
					if err != nil {
						out["error"] = err
					} else {
						if result == nil {
							result = map[string]any{}
						}
						out["result"] = result
					}
					p.w.send(out)
				})
			})
		case m.Method != "":
			if p.onNotify != nil {
				p.onNotify(m.Method, m.Params)
			}
		case len(m.ID) > 0:
			p.mu.Lock()
			ch := p.pending[string(m.ID)]
			delete(p.pending, string(m.ID))
			p.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	})
	p.closeOnce.Do(func() { close(p.closed) })
}

func (p *rpcPeer) notify(method string, params any) {
	out := map[string]any{"method": method, "params": params}
	if p.withVersion {
		out["jsonrpc"] = "2.0"
	}
	p.w.send(out)
}

// call sends a request and waits for the response, the hub going away, or
// abort closing.
func (p *rpcPeer) call(method string, params any, abort <-chan struct{}) (json.RawMessage, error) {
	n := p.seq.Add(1)
	id := json.RawMessage(strconv.FormatInt(1000000+n, 10))
	ch := make(chan rpcMsg, 1)
	p.mu.Lock()
	p.pending[string(id)] = ch
	p.mu.Unlock()
	out := map[string]any{"id": id, "method": method, "params": params}
	if p.withVersion {
		out["jsonrpc"] = "2.0"
	}
	p.w.send(out)
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-p.closed:
		return nil, errors.New("hub went away")
	case <-abort:
		return nil, errors.New("aborted")
	}
}

// ------------------------------------------------------------------ turn runner

// turnRunner runs at most one brain turn at a time and tracks cancellation.
type turnRunner struct {
	b      *Brain
	mu     sync.Mutex
	cancel chan struct{}
	done   chan struct{} // closed when the turn goroutine fully exits
	freed  chan struct{} // closed when the slot is free for the next turn
	free   func()        // releases the current slot (idempotent)
	gone   chan struct{} // closed when stdin ends
}

func newRunner(b *Brain) *turnRunner { return &turnRunner{b: b, gone: make(chan struct{})} }

// start runs fn in a goroutine; false if a turn is already running.
func (r *turnRunner) start(fn func(cancel <-chan struct{})) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.freed != nil {
		select {
		case <-r.freed:
		default:
			return false
		}
	}
	r.cancel = make(chan struct{})
	r.done = make(chan struct{})
	r.freed = make(chan struct{})
	c, d, fr := r.cancel, r.done, r.freed
	var once sync.Once
	free := func() { once.Do(func() { close(fr) }) }
	r.free = free
	go func() {
		defer close(d)
		defer free()
		fn(c)
	}()
	return true
}

// freeSlot lets a driver release the turn slot right before it emits its
// terminal notification, so the host can start the next turn without racing
// the goroutine's deferred close. Idempotent; the deferred free is a no-op
// after it. The done channel still marks the goroutine's true exit.
func (r *turnRunner) freeSlot() {
	r.mu.Lock()
	f := r.free
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

// interrupt cancels the running turn (idempotent). A stubborn harness ignores it.
func (r *turnRunner) interrupt() {
	if r.b.Stubborn() {
		r.b.Event("interrupt_ignored", "")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		select {
		case <-r.cancel:
		default:
			close(r.cancel)
		}
	}
}

// finish is called when stdin closes: well-behaved harnesses let the current
// tool finish and exit; a misbehaving long task never exits by itself.
func (r *turnRunner) finish() {
	close(r.gone)
	if r.b.Stubborn() {
		select {} // only killing the process tree helps
	}
	r.interrupt()
	r.mu.Lock()
	d := r.done
	r.mu.Unlock()
	if d != nil {
		<-d
	}
	r.b.ShutdownChildren()
	r.b.Event("exit", "stdin closed")
}
