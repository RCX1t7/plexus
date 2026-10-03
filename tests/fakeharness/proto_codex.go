package fakeharness

// Codex app-server (`codex app-server`): JSON-RPC over stdio WITHOUT the
// "jsonrpc" field. Shapes from the public app-server README
// (developers.openai.com/codex/app-server, mirrored in hh-research):
//   initialize -> {userAgent}; notify initialized; account/read
//   thread/start{cwd,approvalPolicy,sandbox,developerInstructions,dynamicTools} | thread/resume{threadId} -> {thread:{id}}
//   turn/start{threadId,input:[{type:text,text}],approvalPolicy?,sandboxPolicy?} -> {turn:{id,status}}
//   turn/interrupt{threadId,turnId}; turn/steer (accepted, ignored)
//   notifications: thread/started, turn/started, item/started, item/completed,
//     item/agentMessage/delta, error{error,willRetry}, turn/completed{turn:{id,status,error}}
//   server requests: item/commandExecution/requestApproval, item/fileChange/requestApproval
//     -> {decision: accept|acceptForSession|decline|cancel}; item/tool/requestUserInput
//     -> {answers:{<id>:{answers:[...]}}}; item/tool/call (dynamic tools, experimental)
//     -> {contentItems:[...], success}
// GUESSES: ordering item/started(commandExecution) before its approval request
// (documented for fileChange only); agentMessage phase "final_answer";
// sandbox/approval value spellings "read-only"/"readOnly", "never".

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

type codexDriver struct {
	b        *Brain
	w        *lineWriter
	p        *rpcPeer
	r        *turnRunner
	mu       sync.Mutex
	approval string
	readOnly bool
	tools    map[string]bool
	seq      atomic.Int64
	retried  bool
}

func newCodex(b *Brain, w *lineWriter) *codexDriver {
	d := &codexDriver{b: b, w: w, p: newPeer(w, false), r: newRunner(b), tools: map[string]bool{}}
	d.p.onRequest = d.request
	d.p.onNotify = func(string, json.RawMessage) {}
	return d
}

func (d *codexDriver) Serve(in io.Reader) error {
	d.p.serve(in)
	d.r.finish()
	return nil
}

func (d *codexDriver) id(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), d.seq.Add(1))
}

type codexThreadParams struct {
	ThreadID       string          `json:"threadId"`
	ApprovalPolicy string          `json:"approvalPolicy"`
	Sandbox        string          `json:"sandbox"`
	SandboxPolicy  json.RawMessage `json:"sandboxPolicy"`
	DynamicTools   []struct {
		Name string `json:"name"`
	} `json:"dynamicTools"`
	Input []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"input"`
}

func (d *codexDriver) apply(p codexThreadParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.ApprovalPolicy != "" {
		d.approval = p.ApprovalPolicy
	}
	if p.Sandbox != "" {
		d.readOnly = p.Sandbox == "read-only"
	}
	if len(p.SandboxPolicy) > 0 {
		var sp struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p.SandboxPolicy, &sp)
		if sp.Type != "" {
			d.readOnly = sp.Type == "readOnly"
		}
	}
	for _, t := range p.DynamicTools {
		d.tools[t.Name] = true
	}
}

func (d *codexDriver) request(method string, raw json.RawMessage, reply func(any, *RPCError)) {
	var p codexThreadParams
	_ = json.Unmarshal(raw, &p)
	switch method {
	case "initialize":
		reply(map[string]any{"userAgent": "fake-codex/0.1"}, nil)
	case "account/read":
		reply(map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": false}, nil)
	case "thread/start":
		d.apply(p)
		id := d.b.NewSession()
		reply(map[string]any{"thread": map[string]any{"id": id}}, nil)
		d.p.notify("thread/started", map[string]any{"thread": map[string]any{"id": id}})
	case "thread/resume":
		d.apply(p)
		if err := d.b.Resume(p.ThreadID); err != nil {
			reply(nil, &RPCError{Code: -32600, Message: err.Error()})
			return
		}
		reply(map[string]any{"thread": map[string]any{"id": p.ThreadID}}, nil)
	case "turn/start":
		d.apply(p)
		text := ""
		for _, in := range p.Input {
			if in.Type == "text" {
				text += in.Text
			}
		}
		turn := d.id("turn")
		if !d.r.start(func(cancel <-chan struct{}) { d.turn(turn, text, cancel) }) {
			reply(nil, &RPCError{Code: -32600, Message: "a turn is already running"})
			return
		}
		reply(map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress", "items": []any{}}}, nil)
	case "turn/interrupt":
		d.r.interrupt()
		reply(map[string]any{}, nil)
	case "turn/steer":
		reply(map[string]any{}, nil)
	default:
		reply(nil, &RPCError{Code: -32601, Message: "fake codex: unknown method " + method})
	}
}

func (d *codexDriver) turn(turn, text string, cancel <-chan struct{}) {
	tid := d.b.SessionID()
	d.p.notify("turn/started", map[string]any{"threadId": tid, "turn": map[string]any{"id": turn, "status": "inProgress"}})
	if !d.retried { // a transient error the host retries itself: must NOT end the turn
		d.retried = true
		d.p.notify("error", map[string]any{"threadId": tid, "turnId": turn, "willRetry": true,
			"error": map[string]any{"message": "stream disconnected before completion; retrying 1/5"}})
	}
	io := &codexIO{d: d, turn: turn, cancel: cancel}
	final, err := d.b.Turn(io, text)
	status := "completed"
	if err != nil {
		status = "interrupted"
	} else if final != "" {
		item := d.id("msg")
		d.p.notify("item/started", map[string]any{"threadId": tid, "turnId": turn, "item": map[string]any{"type": "agentMessage", "id": item, "text": ""}})
		d.p.notify("item/completed", map[string]any{"threadId": tid, "turnId": turn,
			"item": map[string]any{"type": "agentMessage", "id": item, "text": final, "phase": "final_answer"}})
	}
	d.r.freeSlot()
	d.p.notify("turn/completed", map[string]any{"threadId": tid, "turn": map[string]any{"id": turn, "status": status, "items": []any{}, "error": nil}})
}

type codexIO struct {
	d      *codexDriver
	turn   string
	cancel <-chan struct{}
	items  map[string]map[string]any
}

func (c *codexIO) Cancelled() <-chan struct{} { return c.cancel }

func (c *codexIO) note(method string, item map[string]any) {
	c.d.p.notify(method, map[string]any{"threadId": c.d.b.SessionID(), "turnId": c.turn, "item": item})
}

func (c *codexIO) Delta(text string) {
	c.d.p.notify("item/agentMessage/delta", map[string]any{"threadId": c.d.b.SessionID(), "turnId": c.turn, "itemId": "delta_" + c.turn, "delta": text})
}

func (c *codexIO) Permit(tc *ToolCall) bool {
	c.d.mu.Lock()
	approval, ro := c.d.approval, c.d.readOnly
	c.d.mu.Unlock()
	if ro && tc.Kind == "shell" && tc.ReadOnly {
		return true // known risk: a read-only sandbox runs "safe" commands without asking
	}
	if ro && tc.Kind != "fetch" {
		return false // read-only sandbox: the real codex refuses itself
	}
	tc.CallID = c.d.id("item")
	var item map[string]any
	method := ""
	params := map[string]any{"threadId": c.d.b.SessionID(), "turnId": c.turn, "itemId": tc.CallID, "reason": tc.Title}
	switch tc.Kind {
	case "write":
		item = map[string]any{"type": "fileChange", "id": tc.CallID, "status": "inProgress",
			"changes": []any{map[string]any{"path": tc.Path, "kind": "add"}}}
		method = "item/fileChange/requestApproval"
	case "shell":
		item = map[string]any{"type": "commandExecution", "id": tc.CallID, "status": "inProgress",
			"command": tc.Command, "cwd": c.d.b.cfg.Workspace}
		method = "item/commandExecution/requestApproval"
		params["command"], params["cwd"] = tc.Command, c.d.b.cfg.Workspace
	default:
		item = map[string]any{"type": "webSearch", "id": tc.CallID, "query": tc.URL}
	}
	if c.items == nil {
		c.items = map[string]map[string]any{}
	}
	c.items[tc.CallID] = item
	c.note("item/started", item)
	if method == "" || approval == "never" {
		return true
	}
	res, err := c.d.p.call(method, params, nil)
	if err != nil {
		return false
	}
	var r struct {
		Decision any `json:"decision"`
	}
	_ = json.Unmarshal(res, &r)
	s, _ := r.Decision.(string)
	return s == "accept" || s == "acceptForSession" || (s == "" && r.Decision != nil) // acceptWithExecpolicyAmendment object
}

func (c *codexIO) ToolDone(tc *ToolCall, status string) {
	item := c.items[tc.CallID]
	if item == nil {
		return
	}
	item["status"] = status
	c.note("item/completed", item)
}

func (c *codexIO) Ask(q Asked) (string, bool) {
	var opts []any
	for _, o := range q.Options {
		opts = append(opts, map[string]any{"label": o, "description": ""})
	}
	res, err := c.d.p.call("item/tool/requestUserInput", map[string]any{"threadId": c.d.b.SessionID(), "turnId": c.turn,
		"itemId": c.d.id("ask"), "questions": []any{map[string]any{"id": q.ID, "header": q.Header, "question": q.Text,
			"isOther": true, "options": opts}}}, nil)
	if err != nil {
		return "", false
	}
	var r struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	}
	_ = json.Unmarshal(res, &r)
	if a := r.Answers[q.ID].Answers; len(a) > 0 && a[0] != "" {
		return a[0], true
	}
	return "", false
}

func (c *codexIO) HostTool(name string, args map[string]any) (bool, bool) {
	c.d.mu.Lock()
	ok := c.d.tools[name]
	c.d.mu.Unlock()
	if !ok {
		return false, false
	}
	call := c.d.id("dyn")
	item := map[string]any{"type": "dynamicToolCall", "id": call, "tool": name, "arguments": args, "status": "inProgress"}
	c.note("item/started", item)
	res, err := c.d.p.call("item/tool/call", map[string]any{"threadId": c.d.b.SessionID(), "turnId": c.turn,
		"callId": call, "tool": name, "arguments": args}, nil)
	var r struct {
		Success *bool `json:"success"`
	}
	if err == nil {
		_ = json.Unmarshal(res, &r)
	}
	success := err == nil && (r.Success == nil || *r.Success)
	item["status"] = map[bool]string{true: "completed", false: "failed"}[success]
	item["success"] = success
	c.note("item/completed", item)
	return success, true
}
