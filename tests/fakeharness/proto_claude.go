package fakeharness

// Claude Code stream-json (`claude --input-format stream-json --output-format
// stream-json --verbose --include-partial-messages --permission-prompt-tool stdio`).
//
// Shapes follow the public Agent SDK wire protocol (claude-agent-sdk-python
// _internal/query.py + types.py, TypeScript SDK sdk.d.ts):
//   hub->cli  {"type":"control_request","request_id":R,"request":{"subtype":"initialize","hooks":{...}}}
//   hub->cli  {"type":"user","message":{"role":"user","content":"..."},"session_id":S,"parent_tool_use_id":null}
//   hub->cli  {"type":"control_request","request_id":R,"request":{"subtype":"interrupt"}}
//   cli->hub  {"type":"control_response","response":{"subtype":"success","request_id":R,"response":{...}}}
//   cli->hub  {"type":"control_request","request_id":R,"request":{"subtype":"hook_callback","callback_id":ID,"input":{...},"tool_use_id":T}}
//   cli->hub  {"type":"control_request","request_id":R,"request":{"subtype":"can_use_tool","tool_name":N,"input":{...},"permission_suggestions":[],"tool_use_id":T}}
//   cli->hub  system/init, stream_event (content_block_delta/text_delta), assistant, user (tool_result), result
// GUESSES (not in public docs, marked so they can be checked against a real
// claude): the exact field set of system/init beyond session_id/tools/model;
// the result subtype after an interrupt ("error_during_execution"); whether
// a PreToolUse hook "allow" skips can_use_tool (we assume it does).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type claudeDriver struct {
	b       *Brain
	w       *lineWriter
	r       *turnRunner
	resume  string
	bypass  bool
	hookIDs []string     // PreToolUse callback ids registered at initialize
	mcp     bool         // the hub mounted an SDK MCP server named "plexus"
	mode    atomic.Value // permission mode (set_permission_mode)
	mu      sync.Mutex
	pending map[string]chan map[string]any
	seq     atomic.Int64
	tool    atomic.Int64
}

func newClaude(b *Brain, w *lineWriter, o DriverOptions) *claudeDriver {
	return &claudeDriver{b: b, w: w, r: newRunner(b), resume: o.ResumeID, bypass: o.Bypass, pending: map[string]chan map[string]any{}}
}

func (d *claudeDriver) Serve(in io.Reader) error {
	if d.resume != "" {
		if err := d.b.Resume(d.resume); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return err // the real CLI exits non-zero on a bad --resume id
		}
	} else {
		d.b.NewSession()
	}
	scanLines(in, d.handle)
	d.mu.Lock()
	for id, ch := range d.pending {
		close(ch)
		delete(d.pending, id)
	}
	d.mu.Unlock()
	d.r.finish()
	return nil
}

func (d *claudeDriver) sid() string { return d.b.SessionID() }

func (d *claudeDriver) handle(line []byte) {
	var m struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Response  json.RawMessage `json:"response"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch m.Type {
	case "control_request":
		var req struct {
			Subtype string   `json:"subtype"`
			Mode    string   `json:"mode"`
			SDKMCP  []string `json:"sdkMcpServers"`
			Hooks   map[string][]struct {
				HookCallbackIDs []string `json:"hookCallbackIds"`
			} `json:"hooks"`
		}
		_ = json.Unmarshal(m.Request, &req)
		switch req.Subtype {
		case "initialize":
			for _, h := range req.Hooks["PreToolUse"] {
				d.hookIDs = append(d.hookIDs, h.HookCallbackIDs...)
			}
			for _, s := range req.SDKMCP {
				d.mcp = d.mcp || s == "plexus"
			}
			d.respond(m.RequestID, map[string]any{"commands": []any{}, "output_style": "default",
				"models": []any{map[string]any{"value": "fake", "displayName": "fake"}}})
		case "interrupt":
			d.r.interrupt()
			d.respond(m.RequestID, map[string]any{})
		case "set_permission_mode":
			d.mode.Store(req.Mode)
			d.respond(m.RequestID, map[string]any{})
		case "stop_task":
			d.r.interrupt()
			d.respond(m.RequestID, map[string]any{})
		default: // set_model, mcp_status, ...
			d.respond(m.RequestID, map[string]any{})
		}
	case "control_response":
		var r struct {
			Subtype   string         `json:"subtype"`
			RequestID string         `json:"request_id"`
			Response  map[string]any `json:"response"`
			Error     string         `json:"error"`
		}
		_ = json.Unmarshal(m.Response, &r)
		d.mu.Lock()
		ch := d.pending[r.RequestID]
		delete(d.pending, r.RequestID)
		d.mu.Unlock()
		if ch != nil {
			if r.Subtype == "error" {
				ch <- map[string]any{"__error": r.Error}
			} else {
				ch <- r.Response
			}
		}
	case "user":
		var um struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(m.Message, &um)
		text := contentText(um.Content)
		if !d.r.start(func(cancel <-chan struct{}) { d.turn(text, cancel) }) {
			// The real CLI queues; Plexus never sends a second turn concurrently.
			fmt.Fprintln(os.Stderr, "fakeharness: user message while a turn is running (ignored)")
		}
	}
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	out := ""
	for _, b := range blocks {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}

func (d *claudeDriver) respond(id string, body map[string]any) {
	d.w.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": body}})
}

// control sends a control_request to the hub and waits for its response
// (nil if the hub went away or answered with an error).
func (d *claudeDriver) control(req map[string]any) map[string]any {
	id := "cli-" + strconv.FormatInt(d.seq.Add(1), 10)
	ch := make(chan map[string]any, 1)
	d.mu.Lock()
	d.pending[id] = ch
	d.mu.Unlock()
	d.w.send(map[string]any{"type": "control_request", "request_id": id, "request": req})
	select {
	case r, ok := <-ch:
		if !ok || r["__error"] != nil {
			return nil
		}
		return r
	case <-d.r.gone:
		return nil
	}
}

func (d *claudeDriver) turn(text string, cancel <-chan struct{}) {
	start := time.Now()
	d.w.send(map[string]any{"type": "system", "subtype": "init", "session_id": d.sid(), "cwd": d.b.cfg.Workspace,
		"tools": []string{"Read", "Write", "Edit", "Bash", "AskUserQuestion", "Task"}, "model": "fake-claude",
		"permissionMode": "default", "slash_commands": []string{"compact", "review"}, "agents": []string{"general-purpose"},
		"skills": []string{}, "mcp_servers": []any{}, "apiKeySource": "none", "output_style": "default"})
	io := &claudeIO{d: d, cancel: cancel}
	final, err := d.b.Turn(io, text)
	if err != nil {
		d.r.freeSlot()
		d.w.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"session_id": d.sid(), "duration_ms": time.Since(start).Milliseconds(), "num_turns": 1})
		return
	}
	if final != "" {
		d.w.send(map[string]any{"type": "assistant", "session_id": d.sid(), "parent_tool_use_id": nil,
			"message": map[string]any{"id": fmt.Sprintf("msg_%d", time.Now().UnixNano()), "role": "assistant", "model": "fake-claude",
				"content": []any{map[string]any{"type": "text", "text": final}}}})
	}
	d.r.freeSlot()
	d.w.send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": final,
		"session_id": d.sid(), "duration_ms": time.Since(start).Milliseconds(), "num_turns": 1, "total_cost_usd": 0})
}

type claudeIO struct {
	d      *claudeDriver
	cancel <-chan struct{}
}

func (c *claudeIO) Cancelled() <-chan struct{} { return c.cancel }

func (c *claudeIO) Delta(text string) {
	c.d.w.send(map[string]any{"type": "stream_event", "session_id": c.d.sid(), "parent_tool_use_id": nil,
		"event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}})
}

func (c *claudeIO) toolUse(name string, input map[string]any) string {
	id := fmt.Sprintf("toolu_%d_%d", os.Getpid(), c.d.tool.Add(1))
	c.d.w.send(map[string]any{"type": "assistant", "session_id": c.d.sid(), "parent_tool_use_id": nil,
		"message": map[string]any{"id": "msg_" + id, "role": "assistant", "model": "fake-claude",
			"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	return id
}

// hook runs the PreToolUse hook; "" = no decision.
func (c *claudeIO) hook(name string, input map[string]any, id string) string {
	if len(c.d.hookIDs) == 0 {
		return ""
	}
	r := c.d.control(map[string]any{"subtype": "hook_callback", "callback_id": c.d.hookIDs[0], "tool_use_id": id,
		"input": map[string]any{"session_id": c.d.sid(), "hook_event_name": "PreToolUse", "cwd": c.d.b.cfg.Workspace,
			"tool_name": name, "tool_input": input, "tool_use_id": id}})
	if r == nil {
		return "deny" // hub gone
	}
	if hso, ok := r["hookSpecificOutput"].(map[string]any); ok {
		if dec, _ := hso["permissionDecision"].(string); dec != "" {
			return dec
		}
	}
	if dec, _ := r["decision"].(string); dec == "block" {
		return "deny"
	}
	return ""
}

func claudeTool(tc *ToolCall) (string, map[string]any) {
	if tc.Kind == "fetch" {
		return "WebFetch", map[string]any{"url": tc.URL, "prompt": "summarise"}
	}
	if tc.Kind == "write" {
		return "Write", map[string]any{"file_path": tc.Path, "content": tc.Content}
	}
	return "Bash", map[string]any{"command": tc.Command, "description": tc.Title}
}

func (c *claudeIO) Permit(tc *ToolCall) bool {
	if c.restricted(tc) {
		return false // plan mode: the real CLI refuses writes and commands itself
	}
	if c.d.bypass {
		name, input := claudeTool(tc)
		tc.CallID = c.toolUse(name, input)
		c.d.b.Event("ungated_tool", tc.Action)
		return true
	}
	name, input := claudeTool(tc)
	tc.CallID = c.toolUse(name, input)
	switch c.hook(name, input, tc.CallID) {
	case "allow":
		return true
	case "deny":
		return false
	}
	r := c.d.control(map[string]any{"subtype": "can_use_tool", "tool_name": name, "input": input,
		"permission_suggestions": []any{}, "tool_use_id": tc.CallID})
	return r != nil && r["behavior"] == "allow"
}

func (c *claudeIO) ToolDone(tc *ToolCall, status string) {
	content := "ok"
	isErr := status != "completed"
	if isErr {
		content = "tool " + status
	}
	c.d.w.send(map[string]any{"type": "user", "session_id": c.d.sid(), "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result",
			"tool_use_id": tc.CallID, "content": content, "is_error": isErr}}}})
}

func (c *claudeIO) Ask(q Asked) (string, bool) {
	var opts []any
	for _, o := range q.Options {
		opts = append(opts, map[string]any{"label": o, "description": ""})
	}
	input := map[string]any{"questions": []any{map[string]any{"question": q.Text, "header": q.Header,
		"options": opts, "multiSelect": false}}}
	id := c.toolUse("AskUserQuestion", input)
	if c.hook("AskUserQuestion", input, id) == "deny" {
		return "", false
	}
	r := c.d.control(map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": input,
		"permission_suggestions": []any{}, "tool_use_id": id})
	if r == nil || r["behavior"] != "allow" {
		return "", false
	}
	upd, _ := r["updatedInput"].(map[string]any)
	ans, _ := upd["answers"].(map[string]any)
	a, _ := ans[q.Text].(string)
	c.ToolDone(&ToolCall{CallID: id}, "completed")
	return a, a != ""
}

// HostTool calls a Plexus tool on the hub's in-process SDK MCP server
// ("mcp_message" control request, as in claude-agent-sdk _internal/query.py).
// GUESS: the real CLI first runs initialize/tools/list over mcp_message; the
// fake goes straight to tools/call.
func (c *claudeIO) HostTool(name string, args map[string]any) (bool, bool) {
	if !c.d.mcp {
		return false, false
	}
	id := c.d.tool.Add(1)
	tu := c.toolUse("mcp__plexus__"+name, args)
	r := c.d.control(map[string]any{"subtype": "mcp_message", "server_name": "plexus",
		"message": map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}}})
	ok := false
	if r != nil {
		if mr, _ := r["mcp_response"].(map[string]any); mr != nil && mr["error"] == nil {
			res, _ := mr["result"].(map[string]any)
			isErr, _ := res["isError"].(bool)
			ok = !isErr
		}
	}
	st := "completed"
	if !ok {
		st = "failed"
	}
	c.ToolDone(&ToolCall{CallID: tu}, st)
	return ok, true
}

func (c *claudeIO) restricted(tc *ToolCall) bool {
	m, _ := c.d.mode.Load().(string)
	return m == "plan" && tc.Kind != "fetch"
}
