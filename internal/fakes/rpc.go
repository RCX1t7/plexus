package fakes

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// server is a tiny JSON-RPC peer for the codex/acp/dsh fakes.
type server struct {
	o       *out
	jsonrpc bool
	mu      sync.Mutex
	seq     int
	waiting map[int]chan json.RawMessage
	notes   chan string // client notifications by method (for cancel)
}

type handler func(s *server, method string, params map[string]any) (any, *rpcErr)

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *server) msg(m map[string]any) {
	if s.jsonrpc {
		m["jsonrpc"] = "2.0"
	}
	s.o.send(m)
}

func (s *server) notify(method string, params any) {
	s.msg(map[string]any{"method": method, "params": params})
}

// call sends a server->client request and waits for the result.
func (s *server) call(method string, params any) map[string]any {
	s.mu.Lock()
	s.seq++
	id := 1000 + s.seq
	ch := make(chan json.RawMessage, 1)
	s.waiting[id] = ch
	s.mu.Unlock()
	s.msg(map[string]any{"id": id, "method": method, "params": params})
	var r map[string]any
	_ = json.Unmarshal(<-ch, &r)
	return r
}

func rpcServer(jsonrpc bool, h handler) {
	s := &server{o: &out{w: bufio.NewWriter(os.Stdout)}, jsonrpc: jsonrpc, waiting: map[int]chan json.RawMessage{},
		notes: make(chan string, 16)}
	for sc := lines(); sc.Scan(); {
		var m struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params map[string]any   `json:"params"`
			Result json.RawMessage  `json:"result"`
			RPC    *string          `json:"jsonrpc"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if !jsonrpc && m.RPC != nil {
			os.Exit(4) // codex omits the jsonrpc member; be strict about it
		}
		appendLog(map[string]any{"method": m.Method, "params": m.Params})
		switch {
		case m.Method == "" && m.ID != nil: // response to our call
			var id int
			_ = json.Unmarshal(*m.ID, &id)
			s.mu.Lock()
			ch := s.waiting[id]
			delete(s.waiting, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
		case m.ID == nil: // notification
			select {
			case s.notes <- m.Method:
			default:
			}
		default:
			id := *m.ID
			method, params := m.Method, m.Params
			go func() {
				res, err := h(s, method, params)
				if err != nil {
					s.msg(map[string]any{"id": id, "error": err})
					return
				}
				s.msg(map[string]any{"id": id, "result": res})
			}()
		}
	}
}

func lastLine(text string) string {
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		return text[i+1:]
	}
	return text
}

var steerCh = make(chan string, 1)

func hostArgs(f []string) map[string]any {
	args := map[string]any{}
	_ = json.Unmarshal([]byte(strings.Join(f[2:], " ")), &args)
	return args
}

func codex(s *server, method string, p map[string]any) (any, *rpcErr) {
	switch method {
	case "initialize":
		return map[string]any{"userAgent": "fake", "platformOs": "linux"}, nil
	case "account/read":
		return map[string]any{"account": map[string]any{"type": "chatgpt"}, "requiresOpenaiAuth": true}, nil
	case "thread/start", "thread/resume":
		id := "th-1"
		if t, ok := p["threadId"].(string); ok {
			id = t
		}
		return map[string]any{"thread": map[string]any{"id": id}}, nil
	case "turn/steer":
		in, _ := p["input"].([]any)
		first, _ := in[0].(map[string]any)
		if toString(p["expectedTurnId"]) != "tu-1" {
			return nil, &rpcErr{Code: -32600, Message: "turn mismatch"}
		}
		steerCh <- lastLine(toString(first["text"]))
		return map[string]any{}, nil
	case "turn/interrupt":
		select {
		case steerCh <- "\x00cancel":
		default:
		}
		s.notify("turn/completed", map[string]any{"threadId": "th-1", "turn": map[string]any{"id": "tu-1", "status": "interrupted"}})
		return map[string]any{}, nil
	case "turn/start":
		in, _ := p["input"].([]any)
		first, _ := in[0].(map[string]any)
		text := lastLine(toString(first["text"]))
		go codexTurn(s, text)
		return map[string]any{"turn": map[string]any{"id": "tu-1", "status": "inProgress"}}, nil
	}
	return nil, &rpcErr{Code: -32601, Message: "unknown " + method}
}

func codexTurn(s *server, text string) {
	th, tu := "th-1", "tu-1"
	reply := "echo: " + text
	f := fields(text)
	switch {
	case len(f) >= 1 && f[0] == "SLOW":
		st := <-steerCh
		if st == "\x00cancel" {
			return // turn/interrupt already completed the turn
		}
		reply = "steered: " + st
	case len(f) >= 2 && f[0] == "HOST":
		r := s.call("item/tool/call", map[string]any{"threadId": th, "turnId": tu, "callId": "dt1", "tool": f[1], "arguments": hostArgs(f)})
		items, _ := r["contentItems"].([]any)
		text := ""
		if len(items) > 0 {
			i0, _ := items[0].(map[string]any)
			text = toString(i0["text"])
		}
		reply = "host:" + text + ":" + toString(r["success"])
	case len(f) >= 1 && f[0] == "RETRY":
		s.notify("error", map[string]any{"threadId": th, "turnId": tu, "willRetry": true, "error": map[string]any{"message": "stream disconnected"}})
	case len(f) >= 3 && f[0] == "TOOL" && f[1] == "shell":
		r := s.call("item/commandExecution/requestApproval", map[string]any{"threadId": th, "turnId": tu, "itemId": "call_1", "command": f[2]})
		reply = "decision:" + toString(r["decision"])
	case len(f) >= 3 && f[0] == "TOOL" && f[1] == "write":
		s.notify("item/started", map[string]any{"threadId": th, "turnId": tu, "item": map[string]any{"type": "fileChange", "id": "fc_1",
			"changes": []any{map[string]any{"path": f[2]}}}})
		r := s.call("item/fileChange/requestApproval", map[string]any{"threadId": th, "turnId": tu, "itemId": "fc_1"})
		reply = "decision:" + toString(r["decision"])
	case len(f) >= 1 && f[0] == "ASK":
		r := s.call("item/tool/requestUserInput", map[string]any{"threadId": th, "turnId": tu, "itemId": "q", "questions": []any{
			map[string]any{"id": "q1", "header": "DB", "question": "Which DB?", "options": []any{map[string]any{"label": "SQLite"}}}}})
		reply = "answers:" + toString(r["answers"])
	}
	s.notify("item/agentMessage/delta", map[string]any{"threadId": th, "turnId": tu, "itemId": "msg_1", "delta": reply[:1]})
	s.notify("item/completed", map[string]any{"threadId": th, "turnId": tu, "item": map[string]any{"type": "agentMessage", "id": "msg_0", "text": "thinking", "phase": "commentary"}})
	s.notify("item/completed", map[string]any{"threadId": th, "turnId": tu, "item": map[string]any{"type": "agentMessage", "id": "msg_1", "text": reply, "phase": "final_answer"}})
	s.notify("turn/completed", map[string]any{"threadId": th, "turn": map[string]any{"id": tu, "status": "completed"}})
}

func acp(s *server, method string, p map[string]any) (any, *rpcErr) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}}, nil
	case "session/new":
		return map[string]any{"sessionId": "acp-1"}, nil
	case "session/resume":
		return map[string]any{}, nil
	case "session/prompt":
		sid := toString(p["sessionId"])
		var text string
		if pr, ok := p["prompt"].([]any); ok && len(pr) > 0 {
			b, _ := pr[0].(map[string]any)
			text = lastLine(toString(b["text"]))
		}
		reply := "echo: " + text
		f := fields(text)
		if len(f) >= 3 && f[0] == "TOOL" {
			s.notify("session/update", map[string]any{"sessionId": sid, "update": map[string]any{"sessionUpdate": "tool_call",
				"toolCallId": "c1", "title": "run", "kind": map[string]string{"read": "read", "write": "edit", "shell": "execute"}[f[1]],
				"locations": []any{map[string]any{"path": f[2]}}, "rawInput": map[string]any{"command": f[2]}}})
			r := s.call("session/request_permission", map[string]any{"sessionId": sid,
				"toolCall": map[string]any{"toolCallId": "c1", "title": f[2], "kind": map[string]string{"read": "read", "write": "edit", "shell": "execute"}[f[1]],
					"locations": []any{map[string]any{"path": f[2]}}, "rawInput": map[string]any{"command": f[2]}},
				"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
			oc, _ := r["outcome"].(map[string]any)
			reply = "outcome:" + toString(oc["optionId"])
		}
		if len(f) >= 1 && f[0] == "SLOW" {
			for m := range s.notes {
				if m == "session/cancel" {
					return map[string]any{"stopReason": "cancelled"}, nil
				}
			}
		}
		s.notify("session/update", map[string]any{"sessionId": sid, "update": map[string]any{"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{"type": "text", "text": reply}}})
		return map[string]any{"stopReason": "end_turn"}, nil
	}
	return nil, &rpcErr{Code: -32601, Message: "unknown " + method}
}

func dsh(s *server, method string, p map[string]any) (any, *rpcErr) {
	switch method {
	case "plexus.initialize":
		return map[string]any{"protocol": 1}, nil
	case "plexus.session.open":
		id := "dsh-1"
		if r := toString(p["resume"]); r != "" {
			id = r
		}
		return map[string]any{"sessionId": id}, nil
	case "plexus.cancel":
		select {
		case steerCh <- "\x00cancel":
		default:
		}
		return map[string]any{}, nil
	case "plexus.steer":
		steerCh <- lastLine(toString(p["text"]))
		return map[string]any{}, nil
	case "plexus.stopTask":
		return map[string]any{}, nil
	case "plexus.control":
		return map[string]any{"name": p["name"]}, nil
	case "plexus.prompt":
		text := lastLine(toString(p["text"]))
		go func() {
			reply := "echo: " + text
			f := fields(text)
			// events first: they race the prompt reply on purpose
			s.notify("plexus.event", map[string]any{"turnId": "bt-1", "kind": "text_delta", "text": "e"})
			if len(f) >= 1 && f[0] == "SLOW" {
				st := <-steerCh
				if st == "\x00cancel" {
					s.notify("plexus.event", map[string]any{"turnId": "bt-1", "kind": "error", "text": "cancelled"})
					return
				}
				reply = "steered: " + st
			}
			if len(f) >= 2 && f[0] == "HOST" {
				r := s.call("plexus.tool", map[string]any{"turnId": "bt-1", "id": "t1", "name": f[1], "arguments": hostArgs(f)})
				reply = "host:" + toString(r["text"]) + ":" + toString(r["isError"])
			}
			if len(f) >= 3 && f[0] == "TOOL" {
				r := s.call("plexus.permission", map[string]any{"turnId": "bt-1", "id": "p1",
					"tool": map[string]any{"name": "fs.read", "kind": f[1], "paths": []string{f[2]}}})
				reply = "allow:" + toString(r["allow"])
			}
			s.notify("plexus.event", map[string]any{"turnId": "bt-1", "kind": "final", "text": reply})
		}()
		return map[string]any{"turnId": "bt-1"}, nil
	}
	return nil, &rpcErr{Code: -32601, Message: "unknown " + method}
}
