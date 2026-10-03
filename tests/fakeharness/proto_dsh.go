package fakeharness

// DSH native bridge (`dsh --profile plexus` + Plexus bridge plugin): JSON-RPC
// 2.0 on stdio WITH "jsonrpc":"2.0". The bridge plugin does not exist yet;
// shapes follow the Plexus skeleton's internal/adapters/dsh (all UNVERIFIED,
// both sides are Plexus-defined):
//   plexus.initialize{protocol:1,client,tools?} -> {protocol:1,capabilities}
//   plexus.session.open{cwd,persona,resume} -> {sessionId}
//   plexus.prompt{sessionId,text,level} -> {turnId}; plexus.cancel{sessionId}; plexus.control
//   notify plexus.event{turnId,id,kind,text,tool,status} (harness.Event JSON)
//   request plexus.permission{turnId,id,tool,reason,options} -> {allow,optionId,always,reason}
//   request plexus.question{turnId,id,questions} -> {answers:{<id>:[...]}}
// PROPOSAL (not in the skeleton): host tools. The hub lists them in
// plexus.initialize params.tools (names); the bridge calls back with
// request plexus.tool{sessionId,turnId,name,arguments} -> {ok}.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

type dshDriver struct {
	b     *Brain
	p     *rpcPeer
	r     *turnRunner
	mu    sync.Mutex
	tools map[string]bool
	level string
	seq   atomic.Int64
}

func newDSH(b *Brain, w *lineWriter) *dshDriver {
	d := &dshDriver{b: b, p: newPeer(w, true), r: newRunner(b), tools: map[string]bool{}}
	d.p.onRequest = d.request
	d.p.onNotify = func(string, json.RawMessage) {}
	return d
}

func (d *dshDriver) Serve(in io.Reader) error {
	d.p.serve(in)
	d.r.finish()
	return nil
}

func (d *dshDriver) id(p string) string { return fmt.Sprintf("%s-%d-%d", p, os.Getpid(), d.seq.Add(1)) }

func (d *dshDriver) request(method string, raw json.RawMessage, reply func(any, *RPCError)) {
	var p struct {
		Protocol  int      `json:"protocol"`
		Tools     []string `json:"tools"`
		Resume    string   `json:"resume"`
		SessionID string   `json:"sessionId"`
		Text      string   `json:"text"`
		Level     string   `json:"level"`
	}
	_ = json.Unmarshal(raw, &p)
	switch method {
	case "plexus.initialize":
		d.mu.Lock()
		for _, t := range p.Tools {
			d.tools[t] = true
		}
		d.mu.Unlock()
		reply(map[string]any{"protocol": 1, "capabilities": map[string]any{"questions": true, "resume": true,
			"hostTools": len(p.Tools) > 0}}, nil)
	case "plexus.session.open":
		if p.Resume != "" {
			if err := d.b.Resume(p.Resume); err != nil {
				reply(nil, &RPCError{Code: -32001, Message: err.Error()})
				return
			}
			reply(map[string]any{"sessionId": p.Resume}, nil)
			return
		}
		reply(map[string]any{"sessionId": d.b.NewSession()}, nil)
	case "plexus.prompt":
		d.mu.Lock()
		d.level = p.Level
		d.mu.Unlock()
		turn := d.id("turn")
		if !d.r.start(func(cancel <-chan struct{}) { d.turn(turn, p.Text, cancel) }) {
			reply(nil, &RPCError{Code: -32002, Message: "a turn is already running"})
			return
		}
		reply(map[string]any{"turnId": turn}, nil)
	case "plexus.cancel":
		d.r.interrupt()
		reply(map[string]any{}, nil)
	case "plexus.control":
		reply(map[string]any{}, nil)
	default:
		reply(nil, &RPCError{Code: -32601, Message: "fake dsh bridge: unknown method " + method})
	}
}

func (d *dshDriver) event(turn string, e map[string]any) {
	e["turnId"] = turn
	if e["id"] == nil {
		e["id"] = d.id("ev")
	}
	d.p.notify("plexus.event", e)
}

func (d *dshDriver) turn(turn, text string, cancel <-chan struct{}) {
	io := &dshIO{d: d, turn: turn, cancel: cancel}
	final, err := d.b.Turn(io, text)
	if err != nil {
		d.event(turn, map[string]any{"kind": "error", "text": "cancelled", "status": "cancelled"})
		return
	}
	if final != "" {
		d.event(turn, map[string]any{"kind": "message", "text": final})
	}
	d.event(turn, map[string]any{"kind": "final", "text": final})
}

type dshIO struct {
	d      *dshDriver
	turn   string
	cancel <-chan struct{}
}

func (c *dshIO) Cancelled() <-chan struct{} { return c.cancel }
func (c *dshIO) Delta(t string)             { c.d.event(c.turn, map[string]any{"kind": "text_delta", "text": t}) }

func dshTool(tc *ToolCall) map[string]any {
	t := map[string]any{"call_id": tc.CallID, "name": map[string]string{"write": "write_file", "shell": "shell", "fetch": "web_fetch"}[tc.Kind], "kind": tc.Kind}
	if tc.Path != "" {
		t["paths"] = []string{tc.Path}
	}
	if tc.Command != "" {
		t["command"] = tc.Command
	}
	if tc.URL != "" {
		t["input"] = map[string]any{"url": tc.URL}
	}
	return t
}

func (c *dshIO) Permit(tc *ToolCall) bool {
	c.d.mu.Lock()
	lv := c.d.level
	c.d.mu.Unlock()
	if (lv == "chat" && tc.Kind != "fetch") || (lv == "readonly" && (tc.Kind == "write" || tc.Kind == "shell")) {
		return false
	}
	tc.CallID = c.d.id("call")
	c.d.event(c.turn, map[string]any{"kind": "tool_use", "id": tc.CallID, "tool": dshTool(tc)})
	res, err := c.d.p.call("plexus.permission", map[string]any{"turnId": c.turn, "id": "perm-" + tc.CallID,
		"tool": dshTool(tc), "reason": tc.Title, "options": []any{}}, nil)
	if err != nil {
		return false
	}
	var r struct {
		Allow bool `json:"allow"`
	}
	_ = json.Unmarshal(res, &r)
	return r.Allow
}

func (c *dshIO) ToolDone(tc *ToolCall, status string) {
	c.d.event(c.turn, map[string]any{"kind": "tool_result", "id": tc.CallID + ":result", "status": status})
}

func (c *dshIO) Ask(q Asked) (string, bool) {
	var opts []any
	for _, o := range q.Options {
		opts = append(opts, map[string]any{"label": o})
	}
	res, err := c.d.p.call("plexus.question", map[string]any{"turnId": c.turn, "id": c.d.id("q"),
		"questions": []any{map[string]any{"id": q.ID, "header": q.Header, "text": q.Text, "options": opts, "free_text": true}}}, nil)
	if err != nil {
		return "", false
	}
	var r struct {
		Answers map[string][]string `json:"answers"`
	}
	_ = json.Unmarshal(res, &r)
	if a := r.Answers[q.ID]; len(a) > 0 && a[0] != "" {
		return a[0], true
	}
	return "", false
}

func (c *dshIO) HostTool(name string, args map[string]any) (bool, bool) {
	c.d.mu.Lock()
	ok := c.d.tools[name]
	c.d.mu.Unlock()
	if !ok {
		return false, false
	}
	res, err := c.d.p.call("plexus.tool", map[string]any{"sessionId": c.d.b.SessionID(), "turnId": c.turn, "name": name, "arguments": args}, nil)
	if err != nil {
		return false, true
	}
	var r struct {
		OK bool `json:"ok"`
	}
	_ = json.Unmarshal(res, &r)
	return r.OK, true
}
