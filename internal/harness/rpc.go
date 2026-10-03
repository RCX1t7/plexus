package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// RPC is a minimal bidirectional JSON-RPC 2.0 peer over a Proc. Codex
// app-server omits the "jsonrpc" field on the wire; ACP requires it.
type RPC struct {
	p       *Proc
	version bool
	mu      sync.Mutex
	next    int64
	waiting map[string]chan rpcMsg
	// OnRequest handles a request from the harness. It must eventually call
	// reply exactly once (it may do so from another goroutine).
	OnRequest func(method string, params json.RawMessage, reply func(result any, err *RPCError))
	// OnNotify handles a notification from the harness.
	OnNotify func(method string, params json.RawMessage)
	closed   chan struct{}
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// NewRPC wraps p. Call Run to start reading after setting the handlers.
func NewRPC(p *Proc, jsonrpcField bool) *RPC {
	return &RPC{p: p, version: jsonrpcField, waiting: map[string]chan rpcMsg{}, closed: make(chan struct{})}
}

// Run reads messages until the process ends.
func (r *RPC) Run() {
	defer close(r.closed)
	for line := range r.p.Lines {
		var m rpcMsg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			id := m.ID
			var once sync.Once
			reply := func(res any, err *RPCError) {
				once.Do(func() { r.send(rpcOut{ID: id, Result: res, Error: err, hasResult: err == nil}) })
			}
			if r.OnRequest == nil {
				reply(nil, &RPCError{Code: -32601, Message: "method not found"})
				continue
			}
			r.OnRequest(m.Method, m.Params, reply)
		case m.Method != "":
			if r.OnNotify != nil {
				r.OnNotify(m.Method, m.Params)
			}
		case len(m.ID) > 0:
			r.mu.Lock()
			ch := r.waiting[string(m.ID)]
			delete(r.waiting, string(m.ID))
			r.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

type rpcOut struct {
	ID        json.RawMessage
	Method    string
	Params    any
	Result    any
	Error     *RPCError
	hasResult bool
}

func (r *RPC) send(o rpcOut) error {
	m := map[string]any{}
	if r.version {
		m["jsonrpc"] = "2.0"
	}
	if o.ID != nil {
		m["id"] = o.ID
	}
	if o.Method != "" {
		m["method"] = o.Method
		if o.Params != nil {
			m["params"] = o.Params
		}
	}
	if o.Error != nil {
		m["error"] = o.Error
	} else if o.hasResult {
		if o.Result == nil {
			m["result"] = map[string]any{}
		} else {
			m["result"] = o.Result
		}
	}
	return r.p.Write(m)
}

// Call sends a request and decodes the result into out (if non-nil).
func (r *RPC) Call(ctx context.Context, method string, params any, out any) error {
	r.mu.Lock()
	r.next++
	id := json.RawMessage(fmt.Sprint(r.next))
	ch := make(chan rpcMsg, 1)
	r.waiting[string(id)] = ch
	r.mu.Unlock()
	if err := r.send(rpcOut{ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-r.closed:
		return errors.New("harness exited")
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.waiting, string(id))
		r.mu.Unlock()
		return ctx.Err()
	}
}

// Notify sends a notification.
func (r *RPC) Notify(method string, params any) error {
	return r.send(rpcOut{Method: method, Params: params})
}

// Closed is closed when the reader stops.
func (r *RPC) Closed() <-chan struct{} { return r.closed }
