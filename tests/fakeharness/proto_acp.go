package fakeharness

// Agent Client Protocol (agentclientprotocol.com, schema in
// hh-research/agent-client-protocol): JSON-RPC 2.0 on stdio. Agent side:
//   initialize{protocolVersion,clientCapabilities,clientInfo} -> {protocolVersion,agentCapabilities{loadSession,sessionCapabilities{resume}},authMethods}
//   session/new{cwd,mcpServers} -> {sessionId}; session/load | session/resume{sessionId,cwd,mcpServers}
//   session/prompt{sessionId,prompt:[{type:text,text}]} -> {stopReason: end_turn|cancelled}
//   notify session/cancel{sessionId}
//   agent->client notify session/update{sessionId,update:{sessionUpdate: agent_message_chunk|agent_thought_chunk|tool_call|tool_call_update,...}}
//   agent->client request session/request_permission{sessionId,toolCall,options[{optionId,name,kind}]} -> {outcome:{outcome:selected,optionId}|{outcome:cancelled}}
// ACP has no user-question or host-tool channel: Ask/HostTool are unsupported
// (Plexus degrades ACP bots to HostTools=Unsupported, ARCHITECTURE §5.2).
// GUESS: session/resume (unstable in the schema) result shape is {}.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

type acpDriver struct {
	b   *Brain
	p   *rpcPeer
	r   *turnRunner
	seq atomic.Int64
}

func newACP(b *Brain, w *lineWriter) *acpDriver {
	d := &acpDriver{b: b, p: newPeer(w, true), r: newRunner(b)}
	d.p.onRequest = d.request
	d.p.onNotify = func(method string, _ json.RawMessage) {
		if method == "session/cancel" {
			d.r.interrupt()
		}
	}
	return d
}

func (d *acpDriver) Serve(in io.Reader) error {
	d.p.serve(in)
	d.r.finish()
	return nil
}

func (d *acpDriver) request(method string, raw json.RawMessage, reply func(any, *RPCError)) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(raw, &p)
	switch method {
	case "initialize":
		reply(map[string]any{"protocolVersion": 1, "authMethods": []any{},
			"agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"resume": map[string]any{}},
				"promptCapabilities": map[string]any{"image": false, "audio": false, "embeddedContext": false}}}, nil)
	case "session/new":
		reply(map[string]any{"sessionId": d.b.NewSession()}, nil)
	case "session/load", "session/resume":
		if err := d.b.Resume(p.SessionID); err != nil {
			reply(nil, &RPCError{Code: -32002, Message: err.Error()})
			return
		}
		reply(map[string]any{}, nil)
	case "session/prompt":
		text := ""
		for _, c := range p.Prompt {
			if c.Type == "text" {
				text += c.Text
			}
		}
		if !d.r.start(func(cancel <-chan struct{}) { d.turn(text, cancel, reply) }) {
			reply(nil, &RPCError{Code: -32000, Message: "a prompt is already running"})
		}
	default:
		reply(nil, &RPCError{Code: -32601, Message: "fake acp: unknown method " + method})
	}
}

func (d *acpDriver) update(u map[string]any) {
	d.p.notify("session/update", map[string]any{"sessionId": d.b.SessionID(), "update": u})
}

func (d *acpDriver) turn(text string, cancel <-chan struct{}, reply func(any, *RPCError)) {
	final, err := d.b.Turn(&acpIO{d: d, cancel: cancel}, text)
	if err != nil {
		d.r.freeSlot()
		reply(map[string]any{"stopReason": "cancelled"}, nil)
		return
	}
	if final != "" {
		d.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": final}})
	}
	d.r.freeSlot()
	reply(map[string]any{"stopReason": "end_turn"}, nil)
}

type acpIO struct {
	d      *acpDriver
	cancel <-chan struct{}
}

func (c *acpIO) Cancelled() <-chan struct{} { return c.cancel }

func (c *acpIO) Delta(t string) {
	c.d.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": t}})
}

func acpCall(tc *ToolCall) map[string]any {
	kind := map[string]string{"write": "edit", "shell": "execute", "fetch": "fetch"}[tc.Kind]
	m := map[string]any{"toolCallId": tc.CallID, "title": tc.Title, "kind": kind, "status": "pending"}
	switch tc.Kind {
	case "write":
		m["locations"] = []any{map[string]any{"path": tc.Path}}
		m["rawInput"] = map[string]any{"path": tc.Path}
	case "shell":
		m["rawInput"] = map[string]any{"command": tc.Command}
	case "fetch":
		m["rawInput"] = map[string]any{"url": tc.URL}
	}
	return m
}

func (c *acpIO) Permit(tc *ToolCall) bool {
	tc.CallID = fmt.Sprintf("call_%d_%d", os.Getpid(), c.d.seq.Add(1))
	call := acpCall(tc)
	u := map[string]any{"sessionUpdate": "tool_call"}
	for k, v := range call {
		u[k] = v
	}
	c.d.update(u)
	res, err := c.d.p.call("session/request_permission", map[string]any{"sessionId": c.d.b.SessionID(), "toolCall": call,
		"options": []any{map[string]any{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"},
			map[string]any{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"}}}, nil)
	if err != nil {
		return false
	}
	var r struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(res, &r)
	return r.Outcome.Outcome == "selected" && r.Outcome.OptionID == "allow-once"
}

func (c *acpIO) ToolDone(tc *ToolCall, status string) {
	c.d.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": tc.CallID, "status": status})
}

func (c *acpIO) Ask(Asked) (string, bool)                     { return "", false }
func (c *acpIO) HostTool(string, map[string]any) (bool, bool) { return false, false }
