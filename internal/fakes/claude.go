package fakes

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
)

// claude speaks the stream-json control protocol.
func claude() {
	o := &out{w: bufio.NewWriter(os.Stdout)}
	var wmu sync.Mutex
	waiting := map[string]chan map[string]any{}
	seq := 0
	ask := func(req map[string]any) map[string]any {
		wmu.Lock()
		seq++
		id := "cli-" + strconv.Itoa(seq)
		ch := make(chan map[string]any, 1)
		waiting[id] = ch
		wmu.Unlock()
		o.send(map[string]any{"type": "control_request", "request_id": id, "request": req})
		return <-ch
	}
	session := "sess-fake"
	if r := os.Getenv("PLEXUS_FAKE_RESUMED"); r != "" {
		session = r
	}
	interrupted := make(chan struct{}, 1)
	turns := make(chan string, 8)
	go func() {
		for text := range turns {
			agents := []string{"general-purpose"}
			if os.Getenv("PLEXUS_FAKE_BARE") == "1" {
				agents = nil
			}
			o.send(map[string]any{"type": "system", "subtype": "init", "session_id": session, "agents": agents,
				"slash_commands": []string{}, "skills": []string{}, "mcp_servers": []any{}})
			reply := "echo: " + text
			f := fields(text)
			switch {
			case len(f) >= 3 && f[0] == "TOOL":
				name := map[string]string{"read": "Read", "write": "Write", "shell": "Bash"}[f[1]]
				input := map[string]any{"file_path": f[2], "command": f[2]}
				o.send(map[string]any{"type": "assistant", "session_id": session, "parent_tool_use_id": nil,
					"message": map[string]any{"id": "m1", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": name, "input": input}}}})
				r := ask(map[string]any{"subtype": "hook_callback", "callback_id": "plexus_pretool", "tool_use_id": "toolu_1",
					"input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": name, "tool_input": input}})
				hso, _ := r["hookSpecificOutput"].(map[string]any)
				reply = "hook:" + toString(hso["permissionDecision"])
				r = ask(map[string]any{"subtype": "can_use_tool", "tool_name": name, "input": input, "tool_use_id": "toolu_1"})
				reply += " can_use_tool:" + toString(r["behavior"])
			case len(f) >= 1 && f[0] == "ASK":
				qs := []any{map[string]any{"question": "Which DB?", "header": "DB", "multiSelect": false,
					"options": []any{map[string]any{"label": "SQLite"}, map[string]any{"label": "Postgres"}}}}
				r := ask(map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": map[string]any{"questions": qs}})
				ui, _ := r["updatedInput"].(map[string]any)
				ans, _ := ui["answers"].(map[string]any)
				reply = "answer:" + toString(r["behavior"]) + ":" + toString(ans["Which DB?"])
			case len(f) >= 1 && f[0] == "SLOW":
				<-interrupted
				o.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
					"session_id": session, "terminal_reason": "aborted_streaming"})
				continue
			case len(f) >= 1 && f[0] == "BG":
				o.send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "task-1", "session_id": session})
			}
			o.send(map[string]any{"type": "stream_event", "session_id": session, "parent_tool_use_id": nil,
				"event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": reply[:1]}}})
			o.send(map[string]any{"type": "assistant", "session_id": session, "parent_tool_use_id": nil,
				"message": map[string]any{"id": "m2", "content": []any{map[string]any{"type": "text", "text": reply}}}})
			o.send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply, "session_id": session})
			if len(f) >= 1 && f[0] == "BG" {
				o.send(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "task-1",
					"status": "completed", "summary": "background done", "session_id": session})
			}
		}
	}()
	for sc := lines(); sc.Scan(); {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "control_request":
			id, _ := m["request_id"].(string)
			req, _ := m["request"].(map[string]any)
			appendLog(req)
			switch req["subtype"] {
			case "initialize":
				o.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id,
					"response": map[string]any{"commands": []any{}, "agents": []any{}}}})
			case "interrupt":
				select {
				case interrupted <- struct{}{}:
				default:
				}
				o.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
			case "stop_task":
				o.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id,
					"response": map[string]any{"stopped": req["task_id"]}}})
			default:
				o.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "unsupported"}})
			}
		case "control_response":
			resp, _ := m["response"].(map[string]any)
			id, _ := resp["request_id"].(string)
			body, _ := resp["response"].(map[string]any)
			wmu.Lock()
			ch := waiting[id]
			delete(waiting, id)
			wmu.Unlock()
			if ch != nil {
				ch <- body
			}
		case "user":
			msg, _ := m["message"].(map[string]any)
			text, _ := msg["content"].(string)
			if i := strings.LastIndex(text, "\n"); i >= 0 {
				text = text[i+1:] // drop the Plexus speaker frame
			}
			turns <- text
		}
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
