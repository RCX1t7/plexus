// Package claude adapts the Claude Code CLI through its headless
// stream-json control protocol:
//
//	claude --input-format stream-json --output-format stream-json --verbose
//	       --include-partial-messages --permission-prompt-tool stdio
//
// Like the official SDKs it does not pass -p (stream-json input implies
// print mode): the headless docs say --bare "will become the default for
// -p", and bare mode drops skills, MCP, hooks, plugins, subagents and
// CLAUDE.md. CLAUDE_CODE_SIMPLE and CLAUDECODE are scrubbed from the child
// env, and every system/init is checked for signs of bare mode anyway.
// Nothing that filters or overrides (skills, agents, systemPrompt) is sent
// in initialize; the persona goes through --append-system-prompt.
//
// Every tool call is gated through a PreToolUse hook callback registered in
// the initialize control request, so even tools Claude Code would
// auto-approve (Read, Grep, ...) reach the Plexus policy. AskUserQuestion is
// answered through the can_use_tool callback.
//
// Plexus host tools (plexus_post, ...) are mounted as an in-process "sdk"
// MCP server named "plexus" (--mcp-config + initialize.sdkMcpServers);
// Claude Code then tunnels MCP JSON-RPC through mcp_message control
// requests. UNVERIFIED against a live Claude Code: the shapes follow the
// Agent SDK wire format.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

const (
	preToolHook = "plexus_pretool"
	stopHook    = "plexus_stop"
)

func init() { harness.Register(Adapter{}) }

// Adapter is the Claude Code harness.
type Adapter struct{}

func (Adapter) Name() string { return "claude_code" }

func (Adapter) Capabilities() harness.Capabilities {
	n := harness.Native
	return harness.Capabilities{PermissionCallback: n, AskUser: n, BackgroundTasks: n, Subagents: n,
		SlashCommands: n, Effort: n, Resume: n, Interrupt: n, StopHook: n, SystemPrompt: n, Control: n,
		PerTaskStop: n, HostTools: n, GuestLock: n}
}

// Detect looks for the CLI and local login evidence only.
func (a Adapter) Detect(ctx context.Context, env harness.Env) harness.DetectionResult {
	r := harness.DetectionResult{Harness: a.Name(), Caps: a.Capabilities(), LoggedIn: harness.LoginUnknown}
	r.Path = harness.FindExecutable(env, env.Getenv("PLEXUS_CLAUDE_EXE"), lookup)
	if r.Path == "" {
		r.LoggedIn = harness.LoginNo
		return r
	}
	r.Installed = true
	if v, err := env.RunVersion(ctx, r.Path, "--version"); err == nil {
		r.Version = harness.FirstLine(v)
	} else {
		r.Error = "version check failed"
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		if env.Getenv(k) != "" {
			r.LoggedIn = harness.LoginYes
			r.Evidence = append(r.Evidence, "env:"+k)
		}
	}
	dir := env.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(env.Home, ".claude")
	}
	if env.Exists(filepath.Join(dir, ".credentials.json")) {
		r.LoggedIn = harness.LoginYes
		r.Evidence = append(r.Evidence, "file:.claude/.credentials.json")
	}
	return r
}

// StartSession launches one long-lived claude process and performs the
// initialize control handshake before returning.
func (a Adapter) StartSession(ctx context.Context, o harness.SessionOptions) (harness.Session, error) {
	exe := o.Exe
	if exe == "" {
		exe = a.Detect(ctx, harness.OSEnv()).Path
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio"}
	if o.ResumeID != "" {
		args = append(args, "--resume="+o.ResumeID) // = form: a dash-leading id is not a flag
	}
	if o.Persona != "" {
		args = append(args, "--append-system-prompt", o.Persona)
	}
	if len(o.HostTools) > 0 {
		args = append(args, "--mcp-config", `{"mcpServers":{"plexus":{"type":"sdk","name":"plexus"}}}`)
	}
	args = append(args, o.Args...)
	p, err := harness.StartProc(ctx, exe, args, o.Workdir, o.Env)
	if err != nil {
		return nil, err
	}
	s := &session{p: p, em: harness.NewEmitter(), pending: map[string]chan map[string]any{},
		tools: o.HostTools, tasks: map[string]bool{}}
	s.id.Store(o.ResumeID)
	go s.read()
	// perTaskStopAffordance: an interrupt then stops only the foreground
	// turn; background agents are stopped one by one with
	// Control("stop_task", {"task_id": ...}).
	init := map[string]any{"subtype": "initialize", "perTaskStopAffordance": true, "hooks": map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": nil, "hookCallbackIds": []string{preToolHook}}},
		"Stop":       []any{map[string]any{"matcher": nil, "hookCallbackIds": []string{stopHook}}},
	}}
	if len(o.HostTools) > 0 {
		init["sdkMcpServers"] = []string{"plexus"}
	}
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := s.control(ictx, init); err != nil {
		s.Close()
		return nil, fmt.Errorf("claude initialize: %w", err)
	}
	return s, nil
}

type session struct {
	p       *harness.Proc
	em      *harness.Emitter
	turn    harness.TurnState
	id      atomic.Value // string
	pmu     sync.Mutex
	pending map[string]chan map[string]any
	seq     atomic.Int64

	tmu      sync.Mutex
	text     strings.Builder // committed root-agent text of the current turn
	sawDelta bool

	tools []harness.ToolSpec
	kmu   sync.Mutex
	tasks map[string]bool // running background task ids
}

// Steer writes a user message while a turn runs; Claude Code folds queued
// user messages into the running turn. UNVERIFIED on a live CLI.
func (s *session) Steer(ctx context.Context, text string) error {
	if id, _ := s.turn.Current(); id == "" {
		return errors.New("no turn running")
	}
	return s.p.Write(map[string]any{"type": "user", "session_id": s.ID(), "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": text}})
}

// BackgroundTasks lists background tasks Claude Code reported as started
// and not yet finished.
func (s *session) BackgroundTasks() []string {
	s.kmu.Lock()
	defer s.kmu.Unlock()
	var out []string
	for id := range s.tasks {
		out = append(out, id)
	}
	return out
}

// StopTask stops one background task (needs perTaskStopAffordance).
func (s *session) StopTask(ctx context.Context, id string) error {
	_, err := s.control(ctx, map[string]any{"subtype": "stop_task", "task_id": id})
	return err
}

func (s *session) ID() string                   { v, _ := s.id.Load().(string); return v }
func (s *session) Events() <-chan harness.Event { return s.em.C() }

func (s *session) Close() error {
	err := s.p.Close()
	s.em.Close()
	return err
}

// control sends a control_request and waits for the matching response.
func (s *session) control(ctx context.Context, req map[string]any) (map[string]any, error) {
	id := "plexus-" + strconv.FormatInt(s.seq.Add(1), 10)
	ch := make(chan map[string]any, 1)
	s.pmu.Lock()
	s.pending[id] = ch
	s.pmu.Unlock()
	defer func() { s.pmu.Lock(); delete(s.pending, id); s.pmu.Unlock() }()
	if err := s.p.Write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r["subtype"] == "error" {
			return nil, fmt.Errorf("%v", r["error"])
		}
		return r, nil
	case <-s.p.Done():
		return nil, errors.New("claude exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Control sends an arbitrary control_request subtype (e.g. set_model,
// set_permission_mode, apply_flag_settings, stop_task).
func (s *session) Control(ctx context.Context, name string, payload json.RawMessage) (json.RawMessage, error) {
	req := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
	}
	req["subtype"] = name
	r, err := s.control(ctx, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func (s *session) Send(ctx context.Context, t harness.Turn) (string, error) {
	id, tctx, ok := s.turn.Begin(ctx)
	if !ok {
		return "", errors.New("a turn is already running")
	}
	s.tmu.Lock()
	s.text.Reset()
	s.sawDelta = false
	s.tmu.Unlock()
	msg := map[string]any{"type": "user", "session_id": s.ID(), "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": t.Text}}
	if err := s.p.Write(msg); err != nil {
		s.turn.End(id)
		return "", err
	}
	go func() {
		<-tctx.Done()
		if s.turn.End(id) { // cancelled by the caller, not finished normally
			ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.control(ictx, map[string]any{"subtype": "interrupt"})
			cancel()
			s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "turn cancelled", SessionID: s.ID()})
		}
	}()
	return id, nil
}

func (s *session) Interrupt(ctx context.Context) error {
	_, err := s.control(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

type message struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	UUID      string          `json:"uuid"`
	SessionID string          `json:"session_id"`
	Parent    *string         `json:"parent_tool_use_id"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	Event     json.RawMessage `json:"event"`
	Message   json.RawMessage `json:"message"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	TaskID    string          `json:"task_id"`
}

type block struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Text      string         `json:"text"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	IsError   bool           `json:"is_error"`
}

func (s *session) read() {
	defer func() {
		if id, _ := s.turn.Current(); s.turn.End(id) {
			s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "claude exited"})
		}
		s.em.Emit(harness.Event{Kind: harness.EventError, Text: "claude exited"})
		s.em.Close()
	}()
	for line := range s.p.Lines {
		var m message
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.SessionID != "" {
			s.id.Store(m.SessionID)
		}
		turnID, tctx := s.turn.Current()
		parent := ""
		if m.Parent != nil {
			parent = *m.Parent
		}
		base := harness.Event{TurnID: turnID, ParentID: parent, SessionID: s.ID(), Raw: line}
		switch m.Type {
		case "control_response":
			var r map[string]any
			_ = json.Unmarshal(m.Response, &r)
			id, _ := r["request_id"].(string)
			s.pmu.Lock()
			ch := s.pending[id]
			s.pmu.Unlock()
			if ch != nil {
				if body, ok := r["response"].(map[string]any); ok && r["subtype"] != "error" {
					ch <- body
				} else {
					ch <- r
				}
			}
		case "control_request":
			s.handleControl(tctx, base, m.RequestID, m.Request)
		case "stream_event":
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal(m.Event, &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
				if parent == "" {
					s.tmu.Lock()
					s.sawDelta = true
					s.tmu.Unlock()
				}
				e := base
				e.Kind, e.Text = harness.EventTextDelta, ev.Delta.Text
				s.em.Emit(e)
			}
		case "assistant", "user":
			var am struct {
				ID      string  `json:"id"`
				Content []block `json:"content"`
			}
			_ = json.Unmarshal(m.Message, &am)
			for i, c := range am.Content {
				e := base
				switch {
				case c.Type == "text" && m.Type == "assistant":
					e.Kind, e.Text = harness.EventMessage, c.Text
					if am.ID != "" {
						e.ID = am.ID + ":" + strconv.Itoa(i)
					}
					if parent == "" {
						s.tmu.Lock()
						s.text.WriteString(c.Text)
						s.tmu.Unlock()
					}
				case c.Type == "tool_use":
					req := toolRequest(c.Name, c.Input)
					req.CallID = c.ID
					e.Kind, e.ID, e.Tool = harness.EventToolUse, c.ID, &req
				case c.Type == "tool_result":
					e.Kind, e.ID, e.Status = harness.EventToolResult, c.ToolUseID+":result", "ok"
					if c.IsError {
						e.Status = "error"
					}
				default:
					continue
				}
				s.em.Emit(e)
			}
		case "system":
			e := base
			switch {
			case m.Subtype == "init":
				e.Kind, e.Name = harness.EventExtension, "system/init"
				if w := bareWarning(line); w != "" {
					s.em.Emit(harness.Event{Kind: harness.EventExtension, Name: "plexus.warning", Text: w, TurnID: turnID, SessionID: s.ID()})
				}
			case strings.HasPrefix(m.Subtype, "task_"):
				e.Kind, e.Name, e.Status = harness.EventBackground, m.TaskID, m.Subtype
				if m.TaskID != "" {
					s.kmu.Lock()
					if m.Subtype == "task_started" {
						s.tasks[m.TaskID] = true
					} else if strings.Contains(m.Subtype, "notification") || strings.Contains(m.Subtype, "completed") ||
						strings.Contains(m.Subtype, "stopped") || strings.Contains(m.Subtype, "failed") {
						delete(s.tasks, m.TaskID)
					}
					s.kmu.Unlock()
				}
				if strings.Contains(m.Subtype, "notification") || strings.Contains(m.Subtype, "completed") {
					e.TurnID = "" // background results may arrive after the turn ended
				}
			default:
				e.Kind, e.Name = harness.EventExtension, "system/"+m.Subtype
			}
			s.em.Emit(e)
		case "result":
			if !s.turn.End(turnID) {
				continue
			}
			e := base
			e.ParentID = ""
			s.tmu.Lock()
			committed := s.text.String()
			s.tmu.Unlock()
			if m.IsError || (m.Subtype != "" && m.Subtype != "success") {
				e.Kind, e.Text = harness.EventError, firstNonEmpty(m.Result, m.Subtype)
			} else {
				e.Kind, e.Text = harness.EventFinal, firstNonEmpty(m.Result, committed)
			}
			s.em.Emit(e)
		default:
			e := base
			e.Kind, e.Name = harness.EventExtension, m.Type
			s.em.Emit(e)
		}
	}
}

func (s *session) respond(id string, body map[string]any) {
	_ = s.p.Write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": body}})
}

func (s *session) respondErr(id, msg string) {
	_ = s.p.Write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": id, "error": msg}})
}

func (s *session) handleControl(tctx context.Context, base harness.Event, id string, raw json.RawMessage) {
	var r struct {
		Subtype     string          `json:"subtype"`
		ToolName    string          `json:"tool_name"`
		Input       map[string]any  `json:"input"`
		CallbackID  string          `json:"callback_id"`
		ToolUseID   string          `json:"tool_use_id"`
		Suggestions []any           `json:"permission_suggestions"`
		Reason      any             `json:"decision_reason"`
		ServerName  string          `json:"server_name"`
		Message     json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(raw, &r)
	base.ID = "req-" + id
	switch r.Subtype {
	case "hook_callback":
		if r.CallbackID == stopHook {
			e := base
			e.Kind, e.Name = harness.EventExtension, "stop_hook"
			s.em.Emit(e)
			s.respond(id, map[string]any{"continue": true})
			return
		}
		name, _ := r.Input["tool_name"].(string)
		in, _ := r.Input["tool_input"].(map[string]any)
		if name == "AskUserQuestion" {
			s.respond(id, map[string]any{"continue": true}) // answered in can_use_tool
			return
		}
		req := toolRequest(name, in)
		req.CallID = r.ToolUseID
		e := base
		e.Perm = &harness.PermissionRequest{Tool: req}
		s.em.Ask(tctx, e, func(d harness.Decision) { s.respond(id, hookDecision(d)) })
	case "can_use_tool":
		if r.ToolName == "AskUserQuestion" {
			qs := parseQuestions(r.Input)
			e := base
			e.Questions = qs
			s.em.AskQuestions(tctx, e, func(ans harness.Answers) {
				if ans == nil {
					s.respond(id, map[string]any{"behavior": "deny", "message": "the user did not answer"})
					return
				}
				out := map[string]any{}
				for _, q := range qs {
					if a, ok := ans[q.ID]; ok {
						out[q.Text] = strings.Join(a, ", ")
					}
				}
				s.respond(id, map[string]any{"behavior": "allow",
					"updatedInput": map[string]any{"questions": r.Input["questions"], "answers": out}})
			})
			return
		}
		req := toolRequest(r.ToolName, r.Input)
		req.CallID = r.ToolUseID
		perm := &harness.PermissionRequest{Tool: req, Reason: fmt.Sprint(firstNonNil(r.Reason, ""))}
		if len(r.Suggestions) > 0 {
			perm.Options = []harness.PermissionOption{{ID: "allow_once", Label: "Allow once", Kind: harness.AllowOnce},
				{ID: "allow_always", Label: "Always allow", Kind: harness.AllowAlways}, {ID: "deny", Label: "Deny", Kind: harness.RejectOnce}}
		}
		e := base
		e.Perm = perm
		input := r.Input
		s.em.Ask(tctx, e, func(d harness.Decision) {
			if !d.Allow {
				s.respond(id, map[string]any{"behavior": "deny", "message": "Plexus policy: " + d.Reason})
				return
			}
			body := map[string]any{"behavior": "allow", "updatedInput": input}
			if d.UpdatedInput != nil {
				body["updatedInput"] = d.UpdatedInput
			}
			if (d.Always || d.OptionID == "allow_always") && len(r.Suggestions) > 0 {
				body["updatedPermissions"] = r.Suggestions
			}
			s.respond(id, body)
		})
	case "mcp_message":
		if r.ServerName != "plexus" {
			s.respondErr(id, "unknown MCP server "+r.ServerName)
			return
		}
		s.mcp(tctx, base, id, r.Message)
	default:
		s.respondErr(id, "unsupported control request: "+r.Subtype)
	}
}

// mcp answers one JSON-RPC message for the in-process "plexus" MCP server.
func (s *session) mcp(tctx context.Context, base harness.Event, id string, raw json.RawMessage) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Name            string         `json:"name"`
			Arguments       map[string]any `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &m)
	reply := func(result any) {
		rid := m.ID
		if len(rid) == 0 {
			rid = json.RawMessage("0")
		}
		s.respond(id, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": rid, "result": result}})
	}
	switch m.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": firstNonEmpty(m.Params.ProtocolVersion, "2025-06-18"),
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": "plexus", "version": "1"}})
	case "tools/list":
		var tools []map[string]any
		for _, t := range s.tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		reply(map[string]any{"tools": tools})
	case "tools/call":
		e := base
		e.Name = m.Params.Name
		e.Tool = &harness.ToolRequest{Name: m.Params.Name, Kind: harness.ToolMeta, Input: m.Params.Arguments, CallID: base.ID}
		s.em.CallTool(tctx, e, func(r harness.HostResult) {
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": r.Text}}, "isError": r.IsError})
		})
	default:
		if strings.HasPrefix(m.Method, "notifications/") {
			reply(map[string]any{})
			return
		}
		s.respond(id, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": m.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"}}})
	}
}

func hookDecision(d harness.Decision) map[string]any {
	dec := "deny"
	if d.Allow {
		dec = "allow"
	}
	out := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": dec,
		"permissionDecisionReason": "Plexus policy: " + d.Reason}
	if d.Allow && d.UpdatedInput != nil {
		out["updatedInput"] = d.UpdatedInput
	}
	return map[string]any{"continue": true, "hookSpecificOutput": out}
}

func parseQuestions(in map[string]any) []harness.Question {
	var out []harness.Question
	list, _ := in["questions"].([]any)
	for i, x := range list {
		q, _ := x.(map[string]any)
		text, _ := q["question"].(string)
		hdr, _ := q["header"].(string)
		multi, _ := q["multiSelect"].(bool)
		var opts []harness.QuestionOption
		if os, ok := q["options"].([]any); ok {
			for _, o := range os {
				if om, ok := o.(map[string]any); ok {
					l, _ := om["label"].(string)
					d, _ := om["description"].(string)
					opts = append(opts, harness.QuestionOption{Label: l, Description: d})
				}
			}
		}
		out = append(out, harness.Question{ID: strconv.Itoa(i), Header: hdr, Text: text, Options: opts, Multi: multi, FreeText: true})
	}
	return out
}

// toolRequest maps a Claude Code tool to a normalized request.
func toolRequest(name string, in map[string]any) harness.ToolRequest {
	str := func(k string) string { v, _ := in[k].(string); return v }
	r := harness.ToolRequest{Name: name, Input: in, Kind: harness.ToolOther}
	switch name {
	case "Read", "NotebookRead":
		r.Kind, r.Paths = harness.ToolRead, nonEmpty(str("file_path"), str("notebook_path"))
	case "Glob", "Grep", "LS":
		r.Kind, r.Paths = harness.ToolRead, []string{firstNonEmpty(str("path"), ".")}
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		r.Kind, r.Paths = harness.ToolWrite, nonEmpty(str("file_path"), str("notebook_path"))
	case "Bash", "BashOutput", "KillShell", "KillBash", "PowerShell":
		r.Kind, r.Command = harness.ToolShell, str("command")
	case "WebFetch", "WebSearch":
		r.Kind = harness.ToolFetch
	case "AskUserQuestion":
		r.Kind = harness.ToolAsk
	case "TodoWrite", "ExitPlanMode", "Agent", "Task", "Skill",
		"mcp__plexus__plexus_post", "mcp__plexus__plexus_delegate", "mcp__plexus__plexus_deliver", "mcp__plexus__plexus_stop_tree":
		r.Kind = harness.ToolMeta
	}
	return r
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstNonNil(v ...any) any {
	for _, x := range v {
		if x != nil {
			return x
		}
	}
	return nil
}

// lookup prefers the native claude.exe; the npm package ships it as bin/claude.exe.
var lookup = harness.Lookup{Names: []string{"claude"}, Npm: []string{
	"@anthropic-ai/claude-code/bin/claude.exe", "@anthropic-ai/claude-code/cli.js"}}

// bareWarning reports a system/init that looks like bare mode: no agents,
// no slash commands, no skills and no MCP servers at all (a normal session
// always lists built-in agents and commands).
func bareWarning(line []byte) string {
	var in struct {
		Agents   []any `json:"agents"`
		Commands []any `json:"slash_commands"`
		Skills   []any `json:"skills"`
		MCP      []any `json:"mcp_servers"`
	}
	if json.Unmarshal(line, &in) != nil {
		return ""
	}
	if len(in.Agents)+len(in.Commands)+len(in.Skills)+len(in.MCP) == 0 {
		return "Claude Code started without agents, commands, skills or MCP servers: it may be running in --bare mode, which strips native features. Check the Claude Code release notes for the opt-out flag and add it to the bot's args."
	}
	return ""
}
