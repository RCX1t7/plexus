// Package hclient is a minimal HUB-SIDE client for the four native harness
// protocols (Claude stream-json, Codex app-server, DSH bridge, ACP). It is
// used by the reference stub hub (refhub) and by the fake harness
// conformance tests. It mirrors the shapes the Plexus skeleton adapters send
// (internal/adapters/*), so the fake is exercised the way Plexus drives it.
// It is test tooling, not a Plexus implementation.
package hclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Claude = "claude"
	Codex  = "codex"
	DSH    = "dsh-bridge"
	ACP    = "acp"
)

// Tool is a normalized permission request.
type Tool struct {
	Name, Kind, Command string
	Paths               []string
}

// Question is a normalized user question.
type Question struct {
	ID, Text string
	Options  []string
}

// Handler answers the harness' requests. All fields are optional.
type Handler struct {
	Permission func(t Tool) bool
	Question   func(q Question) (string, bool)
	HostTool   func(name string, args map[string]any) bool
	Delta      func(text string)
}

// Options configure a session.
type Options struct {
	Exe       string
	Args      []string // extra args (e.g. --sim-role claude)
	Dir       string
	Env       []string
	ResumeID  string
	HostTools []string // Plexus host tool names to mount natively
	Approval  string   // codex approvalPolicy (default "on-request")
	Level     string   // dsh bridge level (default "full")
	Setpgid   bool     // own process group (Linux), for tree kill
}

// Session is one running harness process.
type Session struct {
	proto  string
	cmd    *exec.Cmd
	in     io.WriteCloser
	h      Handler
	wmu    sync.Mutex
	seq    atomic.Int64
	pmu    sync.Mutex
	pend   map[string]chan json.RawMessage
	done   chan struct{}
	id     atomic.Value
	turnCh chan turnEnd
	ccur   atomic.Value // codex current turn id
}

type turnEnd struct {
	final string
	err   error
}

// ErrInterrupted is returned by Prompt when the turn was interrupted.
var ErrInterrupted = errors.New("turn interrupted")

// Start launches the harness and performs the native handshake.
func Start(proto string, o Options, h Handler) (*Session, error) {
	args := nativeArgs(proto, o)
	cmd := exec.Command(o.Exe, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = o.Dir, append(os.Environ(), o.Env...), os.Stderr
	if o.Setpgid {
		setpgid(cmd)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if o.Setpgid {
		attachTree(cmd)
	}
	s := &Session{proto: proto, cmd: cmd, in: in, h: h, pend: map[string]chan json.RawMessage{}, done: make(chan struct{}), turnCh: make(chan turnEnd, 4)}
	s.id.Store("")
	s.ccur.Store("")
	go func() { s.read(out); close(s.done); _ = cmd.Wait() }()
	if err := s.handshake(o); err != nil {
		s.Kill()
		return nil, err
	}
	return s, nil
}

func nativeArgs(proto string, o Options) []string {
	var a []string
	switch proto {
	case Claude:
		a = []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio"}
		if o.ResumeID != "" {
			a = append(a, "--resume="+o.ResumeID)
		}
	case Codex:
		a = []string{"app-server"}
	case DSH:
		a = []string{"--profile", "plexus"}
	case ACP:
		a = []string{"--acp"}
	}
	return append(a, o.Args...)
}

func (s *Session) ID() string { v, _ := s.id.Load().(string); return v }
func (s *Session) Pid() int   { return s.cmd.Process.Pid }

// Done is closed when the harness stdout ends (process exited).
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) write(v any) error {
	b, _ := json.Marshal(v)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.in.Write(append(b, '\n'))
	return err
}

// ------------------------------------------------------------------ requests

func (s *Session) nextID() string { return strconv.FormatInt(s.seq.Add(1), 10) }

func (s *Session) wait(id string, timeout time.Duration) (json.RawMessage, error) {
	s.pmu.Lock()
	ch := s.pend[id]
	s.pmu.Unlock()
	select {
	case r := <-ch:
		var e struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(r, &e) == nil && e.Error != nil {
			return nil, errors.New(e.Error.Message)
		}
		return r, nil
	case <-s.done:
		return nil, errors.New("harness exited")
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting for " + id)
	}
}

func (s *Session) register(id string) {
	s.pmu.Lock()
	s.pend[id] = make(chan json.RawMessage, 1)
	s.pmu.Unlock()
}

func (s *Session) resolve(id string, raw json.RawMessage) {
	s.pmu.Lock()
	ch := s.pend[id]
	delete(s.pend, id)
	s.pmu.Unlock()
	if ch != nil {
		ch <- raw
	}
}

// rpc sends a JSON-RPC request and returns the "result".
func (s *Session) rpc(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := s.nextID()
	s.register(id)
	m := map[string]any{"id": json.RawMessage(id), "method": method, "params": params}
	if s.proto != Codex {
		m["jsonrpc"] = "2.0"
	}
	if err := s.write(m); err != nil {
		return nil, err
	}
	raw, err := s.wait(id, timeout)
	if err != nil {
		return nil, err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.Result, nil
}

func (s *Session) notify(method string, params any) error {
	m := map[string]any{"method": method, "params": params}
	if s.proto != Codex {
		m["jsonrpc"] = "2.0"
	}
	return s.write(m)
}

func (s *Session) reply(id json.RawMessage, result any) {
	m := map[string]any{"id": id, "result": result}
	if s.proto != Codex {
		m["jsonrpc"] = "2.0"
	}
	_ = s.write(m)
}

func (s *Session) control(req map[string]any, timeout time.Duration) (map[string]any, error) {
	id := "hub-" + s.nextID()
	s.register(id)
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, err
	}
	raw, err := s.wait(id, timeout)
	if err != nil {
		return nil, err
	}
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	return r, nil
}

// ------------------------------------------------------------------ handshake

func (s *Session) handshake(o Options) error {
	const t = 60 * time.Second
	switch s.proto {
	case Claude:
		init := map[string]any{"subtype": "initialize", "hooks": map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": nil, "hookCallbackIds": []string{"plexus_pretool"}}}}}
		if len(o.HostTools) > 0 {
			init["sdkMcpServers"] = []string{"plexus"}
		}
		_, err := s.control(init, t)
		if o.ResumeID != "" {
			s.id.Store(o.ResumeID)
		}
		return err
	case Codex:
		if _, err := s.rpc("initialize", map[string]any{"clientInfo": map[string]any{"name": "plexus", "version": "0.1.0"},
			"capabilities": map[string]any{"experimentalApi": true}}, t); err != nil {
			return err
		}
		_ = s.notify("initialized", map[string]any{})
		if _, err := s.rpc("account/read", map[string]any{"refreshToken": false}, t); err != nil {
			return err
		}
		approval := o.Approval
		if approval == "" {
			approval = "on-request"
		}
		var dyn []any
		for _, n := range o.HostTools {
			dyn = append(dyn, map[string]any{"name": n, "description": "Plexus " + n, "inputSchema": map[string]any{"type": "object"}})
		}
		method, params := "thread/start", map[string]any{"cwd": o.Dir, "approvalPolicy": approval, "sandbox": "workspace-write"}
		if len(dyn) > 0 {
			params["dynamicTools"] = dyn
		}
		if o.ResumeID != "" {
			method = "thread/resume"
			params["threadId"] = o.ResumeID
		}
		res, err := s.rpc(method, params, t)
		if err != nil {
			return err
		}
		var r struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(res, &r)
		s.id.Store(r.Thread.ID)
		return nil
	case DSH:
		if _, err := s.rpc("plexus.initialize", map[string]any{"protocol": 1, "client": map[string]any{"name": "plexus"}, "tools": o.HostTools}, t); err != nil {
			return err
		}
		res, err := s.rpc("plexus.session.open", map[string]any{"cwd": o.Dir, "persona": "", "resume": o.ResumeID}, t)
		if err != nil {
			return err
		}
		var r struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(res, &r)
		s.id.Store(r.SessionID)
		return nil
	case ACP:
		if _, err := s.rpc("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, t); err != nil {
			return err
		}
		if o.ResumeID != "" {
			if _, err := s.rpc("session/resume", map[string]any{"sessionId": o.ResumeID, "cwd": o.Dir, "mcpServers": []any{}}, t); err == nil {
				s.id.Store(o.ResumeID)
				return nil
			}
		}
		res, err := s.rpc("session/new", map[string]any{"cwd": o.Dir, "mcpServers": []any{}}, t)
		if err != nil {
			return err
		}
		var r struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(res, &r)
		s.id.Store(r.SessionID)
		return nil
	}
	return fmt.Errorf("unknown protocol %s", s.proto)
}

// ------------------------------------------------------------------ turns

// Prompt runs one turn and blocks until it ends (final text or error).
func (s *Session) Prompt(text, level string) (string, error) {
	for len(s.turnCh) > 0 {
		<-s.turnCh
	}
	switch s.proto {
	case Claude:
		if err := s.write(map[string]any{"type": "user", "session_id": s.ID(), "parent_tool_use_id": nil,
			"message": map[string]any{"role": "user", "content": text}}); err != nil {
			return "", err
		}
	case Codex:
		res, err := s.rpc("turn/start", map[string]any{"threadId": s.ID(), "input": []any{map[string]any{"type": "text", "text": text}}}, 60*time.Second)
		if err != nil {
			return "", err
		}
		var r struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(res, &r)
		s.ccur.Store(r.Turn.ID)
	case DSH:
		if level == "" {
			level = "full"
		}
		if _, err := s.rpc("plexus.prompt", map[string]any{"sessionId": s.ID(), "text": text, "level": level}, 60*time.Second); err != nil {
			return "", err
		}
	case ACP:
		go func() {
			res, err := s.rpc("session/prompt", map[string]any{"sessionId": s.ID(), "prompt": []any{map[string]any{"type": "text", "text": text}}}, 24*time.Hour)
			if err != nil {
				s.turnCh <- turnEnd{err: err}
				return
			}
			var r struct {
				StopReason string `json:"stopReason"`
			}
			_ = json.Unmarshal(res, &r)
			if r.StopReason == "cancelled" {
				s.turnCh <- turnEnd{err: ErrInterrupted}
				return
			}
			s.turnCh <- turnEnd{final: s.takeACPText()}
		}()
	}
	select {
	case e := <-s.turnCh:
		return e.final, e.err
	case <-s.done:
		return "", errors.New("harness exited")
	}
}

// Interrupt sends the native interrupt (and stop_task for Claude).
func (s *Session) Interrupt() error {
	switch s.proto {
	case Claude:
		_, err := s.control(map[string]any{"subtype": "interrupt"}, 5*time.Second)
		return err
	case Codex:
		cur, _ := s.ccur.Load().(string)
		_, err := s.rpc("turn/interrupt", map[string]any{"threadId": s.ID(), "turnId": cur}, 5*time.Second)
		return err
	case DSH:
		_, err := s.rpc("plexus.cancel", map[string]any{"sessionId": s.ID()}, 5*time.Second)
		return err
	case ACP:
		return s.notify("session/cancel", map[string]any{"sessionId": s.ID()})
	}
	return nil
}

// CloseStdin ends the session gracefully; a well-behaved harness exits.
func (s *Session) CloseStdin() { _ = s.in.Close() }

// Exited waits up to d for the process to exit.
func (s *Session) Exited(d time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(d):
		return false
	}
}

// Kill kills the harness (its whole process group / Job Object when Setpgid
// was used).
func (s *Session) Kill() {
	killTree(s.cmd)
	_ = s.in.Close()
}

// ------------------------------------------------------------------ reader

var acpText sync.Map // *Session -> *string accumulated agent_message_chunk text

func (s *Session) takeACPText() string {
	v, _ := acpText.LoadAndDelete(s)
	if p, ok := v.(*string); ok {
		return *p
	}
	return ""
}

func (s *Session) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if s.proto == Claude {
			s.claudeLine(line)
		} else {
			s.rpcLine(line)
		}
	}
	select {
	case s.turnCh <- turnEnd{err: errors.New("harness exited")}:
	default:
	}
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func (s *Session) claudeLine(line []byte) {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if sid := str(m, "session_id"); sid != "" {
		s.id.Store(sid)
	}
	switch str(m, "type") {
	case "control_response":
		r, _ := m["response"].(map[string]any)
		body, _ := json.Marshal(r["response"])
		if str(r, "subtype") == "error" {
			body, _ = json.Marshal(map[string]any{"error": map[string]any{"message": str(r, "error")}})
		}
		s.resolve(str(r, "request_id"), body)
	case "control_request":
		go s.claudeRequest(str(m, "request_id"), m["request"].(map[string]any))
	case "stream_event":
		ev, _ := m["event"].(map[string]any)
		d, _ := ev["delta"].(map[string]any)
		if s.h.Delta != nil && str(d, "type") == "text_delta" {
			s.h.Delta(str(d, "text"))
		}
	case "result":
		if str(m, "subtype") != "success" || m["is_error"] == true {
			s.turnCh <- turnEnd{err: ErrInterrupted}
		} else {
			s.turnCh <- turnEnd{final: str(m, "result")}
		}
	}
}

func (s *Session) respond(id string, body map[string]any) {
	_ = s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": body}})
}

func toStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func (s *Session) claudeRequest(id string, req map[string]any) {
	switch str(req, "subtype") {
	case "hook_callback":
		in, _ := req["input"].(map[string]any)
		name := str(in, "tool_name")
		if name == "AskUserQuestion" {
			s.respond(id, map[string]any{"continue": true})
			return
		}
		ti, _ := in["tool_input"].(map[string]any)
		t := Tool{Name: name, Kind: map[string]string{"Write": "write", "Edit": "write", "Bash": "shell", "WebFetch": "fetch"}[name], Command: str(ti, "command")}
		if p := str(ti, "file_path"); p != "" {
			t.Paths = []string{p}
		}
		dec := "allow"
		if s.h.Permission != nil && !s.h.Permission(t) {
			dec = "deny"
		}
		s.respond(id, map[string]any{"continue": true, "hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": dec}})
	case "can_use_tool":
		in, _ := req["input"].(map[string]any)
		if str(req, "tool_name") == "AskUserQuestion" {
			qs, _ := in["questions"].([]any)
			q0, _ := qs[0].(map[string]any)
			q := Question{ID: "0", Text: str(q0, "question")}
			if os, ok := q0["options"].([]any); ok {
				for _, o := range os {
					om, _ := o.(map[string]any)
					q.Options = append(q.Options, str(om, "label"))
				}
			}
			if s.h.Question == nil {
				s.respond(id, map[string]any{"behavior": "deny", "message": "no answer"})
				return
			}
			a, ok := s.h.Question(q)
			if !ok {
				s.respond(id, map[string]any{"behavior": "deny", "message": "no answer"})
				return
			}
			s.respond(id, map[string]any{"behavior": "allow", "updatedInput": map[string]any{"questions": qs, "answers": map[string]any{q.Text: a}}})
			return
		}
		s.respond(id, map[string]any{"behavior": "allow", "updatedInput": in})
	case "mcp_message":
		msg, _ := req["message"].(map[string]any)
		p, _ := msg["params"].(map[string]any)
		args, _ := p["arguments"].(map[string]any)
		ok := s.h.HostTool != nil && s.h.HostTool(str(p, "name"), args)
		s.respond(id, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": msg["id"],
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "isError": !ok}}})
	default:
		s.respond(id, map[string]any{})
	}
}

func (s *Session) rpcLine(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		go s.rpcRequest(m.ID, m.Method, m.Params)
	case m.Method != "":
		s.rpcNotify(m.Method, m.Params)
	case len(m.ID) > 0:
		s.resolve(string(m.ID), line)
	}
}

func (s *Session) rpcNotify(method string, raw json.RawMessage) {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	switch s.proto {
	case Codex:
		switch method {
		case "item/agentMessage/delta":
			if s.h.Delta != nil {
				s.h.Delta(str(p, "delta"))
			}
		case "item/completed":
			it, _ := p["item"].(map[string]any)
			if str(it, "type") == "agentMessage" {
				acpStore(s, str(it, "text"), true)
			}
		case "turn/completed":
			t, _ := p["turn"].(map[string]any)
			if cur, _ := s.ccur.Load().(string); str(t, "id") != cur {
				return
			}
			if str(t, "status") == "completed" {
				s.turnCh <- turnEnd{final: s.takeACPText()}
			} else {
				s.takeACPText()
				s.turnCh <- turnEnd{err: ErrInterrupted}
			}
		}
	case DSH:
		if method != "plexus.event" {
			return
		}
		switch str(p, "kind") {
		case "text_delta":
			if s.h.Delta != nil {
				s.h.Delta(str(p, "text"))
			}
		case "final":
			s.turnCh <- turnEnd{final: str(p, "text")}
		case "error":
			s.turnCh <- turnEnd{err: ErrInterrupted}
		}
	case ACP:
		if method != "session/update" {
			return
		}
		u, _ := p["update"].(map[string]any)
		switch str(u, "sessionUpdate") {
		case "agent_message_chunk":
			c, _ := u["content"].(map[string]any)
			acpStore(s, str(c, "text"), false)
		case "agent_thought_chunk":
			c, _ := u["content"].(map[string]any)
			if s.h.Delta != nil {
				s.h.Delta(str(c, "text"))
			}
		}
	}
}

func acpStore(s *Session, text string, replace bool) {
	v, _ := acpText.LoadOrStore(s, new(string))
	p := v.(*string)
	if replace {
		*p = text
	} else {
		*p += text
	}
}

func (s *Session) rpcRequest(id json.RawMessage, method string, raw json.RawMessage) {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	perm := func(t Tool) bool { return s.h.Permission == nil || s.h.Permission(t) }
	switch method {
	case "item/commandExecution/requestApproval":
		d := "decline"
		if perm(Tool{Name: "commandExecution", Kind: "shell", Command: fmt.Sprint(p["command"])}) {
			d = "accept"
		}
		s.reply(id, map[string]any{"decision": d})
	case "item/fileChange/requestApproval":
		d := "decline"
		if perm(Tool{Name: "fileChange", Kind: "write"}) {
			d = "accept"
		}
		s.reply(id, map[string]any{"decision": d})
	case "item/tool/requestUserInput":
		qs, _ := p["questions"].([]any)
		out := map[string]any{}
		for _, x := range qs {
			qm, _ := x.(map[string]any)
			q := Question{ID: str(qm, "id"), Text: str(qm, "question")}
			if s.h.Question != nil {
				if a, ok := s.h.Question(q); ok {
					out[q.ID] = map[string]any{"answers": []string{a}}
				}
			}
		}
		s.reply(id, map[string]any{"answers": out})
	case "item/tool/call":
		args, _ := p["arguments"].(map[string]any)
		ok := s.h.HostTool != nil && s.h.HostTool(str(p, "tool"), args)
		s.reply(id, map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": "ok"}}, "success": ok})
	case "plexus.permission":
		t, _ := p["tool"].(map[string]any)
		tool := Tool{Name: str(t, "name"), Kind: str(t, "kind"), Command: str(t, "command"), Paths: toStrings(t["paths"])}
		s.reply(id, map[string]any{"allow": perm(tool)})
	case "plexus.question":
		qs, _ := p["questions"].([]any)
		out := map[string]any{}
		for _, x := range qs {
			qm, _ := x.(map[string]any)
			q := Question{ID: str(qm, "id"), Text: str(qm, "text")}
			if s.h.Question != nil {
				if a, ok := s.h.Question(q); ok {
					out[q.ID] = []string{a}
				}
			}
		}
		s.reply(id, map[string]any{"answers": out})
	case "plexus.tool":
		args, _ := p["arguments"].(map[string]any)
		s.reply(id, map[string]any{"ok": s.h.HostTool != nil && s.h.HostTool(str(p, "name"), args)})
	case "session/request_permission":
		tc, _ := p["toolCall"].(map[string]any)
		kind := map[string]string{"edit": "write", "execute": "shell", "fetch": "fetch"}[str(tc, "kind")]
		opt := "reject-once"
		if perm(Tool{Name: str(tc, "title"), Kind: kind}) {
			opt = "allow-once"
		}
		s.reply(id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": opt}})
	default:
		m := map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "not handled: " + method}}
		if s.proto != Codex {
			m["jsonrpc"] = "2.0"
		}
		_ = s.write(m)
	}
}

// SetMode switches Claude's permission mode (e.g. "plan" for a stranger
// turn, "default" afterwards). Other protocols: no-op.
func (s *Session) SetMode(mode string) error {
	if s.proto != Claude {
		return nil
	}
	_, err := s.control(map[string]any{"subtype": "set_permission_mode", "mode": mode}, 5*time.Second)
	return err
}
