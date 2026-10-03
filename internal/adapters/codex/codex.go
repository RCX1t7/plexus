// Package codex adapts the OpenAI Codex CLI through its long-lived
// `codex app-server` JSON-RPC protocol (JSONL on stdio, "jsonrpc" omitted).
// Approvals (commandExecution / fileChange / permissions) and
// requestUserInput questions are routed to the Plexus policy.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

func init() { harness.Register(Adapter{}) }

// Adapter is the Codex harness.
type Adapter struct{}

func (Adapter) Name() string { return "codex" }

func (Adapter) Capabilities() harness.Capabilities {
	n, u := harness.Native, harness.Unsupported
	return harness.Capabilities{PermissionCallback: n, AskUser: n, BackgroundTasks: n, Subagents: n,
		SlashCommands: u, Effort: n, Resume: n, Interrupt: n, StopHook: u, SystemPrompt: n, Control: n}
}

// Detect: `codex --version` plus auth.json existence. No app-server start.
func (a Adapter) Detect(ctx context.Context, env harness.Env) harness.DetectionResult {
	r := harness.DetectionResult{Harness: a.Name(), Caps: a.Capabilities(), LoggedIn: harness.LoginNo}
	r.Path = harness.FindExecutable(env, env.Getenv("PLEXUS_CODEX_EXE"), lookup)
	if r.Path == "" {
		return r
	}
	r.Installed = true
	if v, err := env.RunVersion(ctx, r.Path, "--version"); err == nil {
		r.Version = harness.FirstLine(v)
	} else {
		r.Error = "version check failed"
	}
	home := env.Getenv("CODEX_HOME")
	if home == "" {
		home = filepath.Join(env.Home, ".codex")
	}
	if env.Exists(filepath.Join(home, "auth.json")) {
		r.LoggedIn = harness.LoginYes
		r.Evidence = append(r.Evidence, "file:.codex/auth.json")
	}
	for _, k := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
		if env.Getenv(k) != "" {
			r.LoggedIn = harness.LoginYes
			r.Evidence = append(r.Evidence, "env:"+k)
		}
	}
	return r
}

// Native sandbox / approval settings per Plexus level (defense in depth;
// the policy callback still decides every approval request).
func sandboxFor(l harness.Level, workdir string) (approval string, policy map[string]any) {
	restricted := map[string]any{"type": "restricted", "includePlatformDefaults": true, "readableRoots": []string{workdir}}
	switch l {
	case harness.LevelFull:
		return "on-request", map[string]any{"type": "workspaceWrite", "writableRoots": []string{workdir}, "networkAccess": true}
	case harness.LevelReadOnly:
		return "untrusted", map[string]any{"type": "readOnly", "access": restricted}
	default:
		return "untrusted", map[string]any{"type": "readOnly", "access": restricted}
	}
}

func (a Adapter) StartSession(ctx context.Context, o harness.SessionOptions) (harness.Session, error) {
	exe := o.Exe
	if exe == "" {
		exe = a.Detect(ctx, harness.OSEnv()).Path
	}
	p, err := harness.StartProc(ctx, exe, append([]string{"app-server"}, o.Args...), o.Workdir, o.Env)
	if err != nil {
		return nil, err
	}
	s := &session{p: p, rpc: harness.NewRPC(p, false), em: harness.NewEmitter(), workdir: o.Workdir, files: map[string][]string{}}
	s.rpc.OnNotify = s.notify
	s.rpc.OnRequest = s.request
	go func() { s.rpc.Run(); s.exited() }()
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fail := func(err error) (harness.Session, error) { s.Close(); return nil, err }
	if err := s.rpc.Call(cctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "plexus", "title": "Plexus", "version": "0.1.0"}}, nil); err != nil {
		return fail(fmt.Errorf("codex initialize: %w", err))
	}
	if err := s.rpc.Notify("initialized", map[string]any{}); err != nil {
		return fail(err)
	}
	var acct struct {
		Account  *map[string]any `json:"account"`
		Requires bool            `json:"requiresOpenaiAuth"`
	}
	if err := s.rpc.Call(cctx, "account/read", map[string]any{"refreshToken": false}, &acct); err == nil && acct.Account == nil && acct.Requires {
		return fail(errors.New("codex is not logged in (run `codex login`)"))
	}
	approval, _ := sandboxFor(harness.LevelChat, o.Workdir)
	params := map[string]any{"cwd": o.Workdir, "approvalPolicy": approval, "sandbox": "read-only"}
	if o.Persona != "" {
		params["developerInstructions"] = o.Persona
	}
	method := "thread/start"
	if o.ResumeID != "" {
		method, params["threadId"] = "thread/resume", o.ResumeID
	}
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := s.rpc.Call(cctx, method, params, &res); err != nil {
		return fail(fmt.Errorf("codex %s: %w", method, err))
	}
	s.thread = res.Thread.ID
	return s, nil
}

type session struct {
	p       *harness.Proc
	rpc     *harness.RPC
	em      *harness.Emitter
	turn    harness.TurnState
	workdir string
	thread  string

	mu     sync.Mutex
	native string // native turn id of the running turn
	turnID string // Plexus turn id
	last   string // last committed agentMessage text
	final  string // agentMessage text with phase final_answer
	errMsg string
	files  map[string][]string // fileChange itemId -> paths
}

func (s *session) ID() string                   { return s.thread }
func (s *session) Events() <-chan harness.Event { return s.em.C() }

func (s *session) Close() error {
	err := s.p.Close()
	s.em.Close()
	return err
}

func (s *session) exited() {
	if id, _ := s.turn.Current(); s.turn.End(id) {
		s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "codex exited"})
	}
	s.em.Emit(harness.Event{Kind: harness.EventError, Text: "codex exited"})
	s.em.Close()
}

func (s *session) Send(ctx context.Context, t harness.Turn) (string, error) {
	id, tctx, ok := s.turn.Begin(ctx)
	if !ok {
		return "", errors.New("a turn is already running")
	}
	s.mu.Lock()
	s.turnID, s.last, s.final, s.errMsg, s.native = id, "", "", "", ""
	s.mu.Unlock()
	approval, sandbox := sandboxFor(t.Level, s.workdir)
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	params := map[string]any{"threadId": s.thread,
		"input": []any{map[string]any{"type": "text", "text": t.Text}}, "cwd": s.workdir,
		"approvalPolicy": approval, "sandboxPolicy": sandbox}
	err := s.rpc.Call(tctx, "turn/start", params, &res)
	var re *harness.RPCError
	if err != nil && errors.As(err, &re) && re.Code == -32602 && sandbox["access"] != nil {
		// Older schema without readOnly.access: fall back to plain readOnly.
		params["sandboxPolicy"] = map[string]any{"type": "readOnly"}
		err = s.rpc.Call(tctx, "turn/start", params, &res)
	}
	if err != nil {
		s.turn.End(id)
		return "", err
	}
	s.mu.Lock()
	if s.native == "" {
		s.native = res.Turn.ID
	}
	s.mu.Unlock()
	go func() {
		<-tctx.Done()
		if s.turn.End(id) {
			ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.interruptNative(ictx, res.Turn.ID)
			cancel()
			s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "turn cancelled", SessionID: s.thread})
		}
	}()
	return id, nil
}

func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	n := s.native
	s.mu.Unlock()
	return s.interruptNative(ctx, n)
}

func (s *session) interruptNative(ctx context.Context, native string) error {
	return s.rpc.Call(ctx, "turn/interrupt", map[string]any{"threadId": s.thread, "turnId": native}, nil)
}

// Control calls any app-server method (e.g. "model/list", "turn/steer").
func (s *session) Control(ctx context.Context, name string, payload json.RawMessage) (json.RawMessage, error) {
	var params any = map[string]any{}
	if len(payload) > 0 {
		params = payload
	}
	var out json.RawMessage
	err := s.rpc.Call(ctx, name, params, &out)
	return out, err
}

type item struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Phase   string `json:"phase"`
	Status  string `json:"status"`
	Command any    `json:"command"`
	Changes []struct {
		Path string `json:"path"`
	} `json:"changes"`
	Tool             string `json:"tool"`
	Server           string `json:"server"`
	Query            string `json:"query"`
	SenderThreadID   string `json:"senderThreadId"`
	ReceiverThreadID string `json:"receiverThreadId"`
}

type envelope struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	Item     item   `json:"item"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	WillRetry bool `json:"willRetry"`
}

// base builds an event for a notification; events from other threads
// (spawned sub-agents) carry that thread as ParentID lineage.
func (s *session) base(p envelope, raw json.RawMessage) harness.Event {
	cur, _ := s.turn.Current()
	s.mu.Lock()
	if tid := firstNonEmpty(p.TurnID, p.Turn.ID); s.native == "" && cur != "" && cur == s.turnID && tid != "" && p.ThreadID == s.thread {
		s.native = tid // a notification raced ahead of the turn/start reply
	}
	turnID, native := s.turnID, s.native
	s.mu.Unlock()
	e := harness.Event{SessionID: s.thread, Raw: raw}
	if cur != "" && (p.TurnID == "" || p.TurnID == native || p.Turn.ID == native) {
		e.TurnID = turnID
	}
	if p.ThreadID != "" && p.ThreadID != s.thread {
		e.ParentID = p.ThreadID
	}
	return e
}

func (s *session) notify(method string, raw json.RawMessage) {
	var p envelope
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	e := s.base(p, raw)
	switch method {
	case "item/agentMessage/delta":
		e.Kind, e.ID, e.Text = harness.EventTextDelta, p.ItemID+":delta", p.Delta
		s.em.Emit(e)
	case "item/started", "item/completed":
		it := p.Item
		e.ID = it.ID
		switch it.Type {
		case "agentMessage":
			if method != "item/completed" {
				return
			}
			s.mu.Lock()
			if e.ParentID == "" {
				s.last = it.Text
				if it.Phase == "final_answer" {
					s.final += it.Text
				}
			}
			s.mu.Unlock()
			e.Kind, e.Text, e.Status = harness.EventMessage, it.Text, it.Phase
		case "fileChange", "commandExecution", "mcpToolCall", "webSearch", "dynamicToolCall", "collabAgentToolCall", "collabToolCall":
			r := itemRequest(it)
			if it.Type == "fileChange" {
				s.mu.Lock()
				s.files[it.ID] = r.Paths
				s.mu.Unlock()
			}
			e.Tool, e.Status = &r, it.Status
			e.Kind = harness.EventToolUse
			if method == "item/completed" {
				e.Kind, e.ID = harness.EventToolResult, it.ID+":result"
			}
		default:
			e.Kind, e.Name = harness.EventExtension, method+"/"+it.Type
		}
		s.em.Emit(e)
	case "error":
		if p.WillRetry { // transient: Codex retries by itself; the turn goes on
			e.Kind, e.Name, e.Text = harness.EventExtension, "codex/error_retrying", p.Error.Message
			s.em.Emit(e)
			return
		}
		s.mu.Lock()
		s.errMsg = p.Error.Message
		s.mu.Unlock()
	case "turn/completed":
		s.mu.Lock()
		native, turnID, text, errMsg := s.native, s.turnID, firstNonEmpty(s.final, s.last), s.errMsg
		s.mu.Unlock()
		if p.Turn.ID != "" && p.Turn.ID != native {
			e.Kind, e.Name = harness.EventBackground, "turn/completed"
			e.Status = p.Turn.Status
			s.em.Emit(e) // a sub-agent or background turn
			return
		}
		if !s.turn.End(turnID) {
			return
		}
		e.TurnID, e.ParentID = turnID, ""
		if p.Turn.Status == "completed" {
			e.Kind, e.Text = harness.EventFinal, text
		} else {
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				errMsg = p.Turn.Error.Message
			}
			e.Kind, e.Text = harness.EventError, firstNonEmpty(errMsg, "turn "+p.Turn.Status)
		}
		s.em.Emit(e)
	default:
		e.Kind, e.Name = harness.EventExtension, method
		s.em.Emit(e)
	}
}

func itemRequest(it item) harness.ToolRequest {
	r := harness.ToolRequest{CallID: it.ID, Name: it.Type}
	switch it.Type {
	case "commandExecution":
		r.Kind, r.Command = harness.ToolShell, commandString(it.Command)
	case "fileChange":
		r.Kind = harness.ToolWrite
		for _, c := range it.Changes {
			r.Paths = append(r.Paths, c.Path)
		}
	case "webSearch":
		r.Kind, r.Input = harness.ToolFetch, it.Query
	default:
		r.Kind, r.Name = harness.ToolOther, firstNonEmpty(strings.Trim(it.Server+"/"+it.Tool, "/"), it.Type)
	}
	return r
}

func commandString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, " ")
	}
	return ""
}

var codexOptions = []harness.PermissionOption{
	{ID: "accept", Label: "Accept", Kind: harness.AllowOnce},
	{ID: "acceptForSession", Label: "Accept for session", Kind: harness.AllowAlways},
	{ID: "decline", Label: "Decline", Kind: harness.RejectOnce},
}

func (s *session) request(method string, raw json.RawMessage, reply func(any, *harness.RPCError)) {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	e := s.base(env, raw)
	_, tctx := s.turn.Current()
	reason, _ := p["reason"].(string)
	ask := func(req harness.ToolRequest, opts []harness.PermissionOption, answer func(harness.Decision) any) {
		ev := e
		ev.ID = "req-" + firstNonEmpty(env.ItemID, method)
		ev.Perm = &harness.PermissionRequest{Tool: req, Reason: reason, Options: opts}
		s.em.Ask(tctx, ev, func(d harness.Decision) { reply(answer(d), nil) })
	}
	decision := func(yes, always, no string) func(harness.Decision) any {
		return func(d harness.Decision) any {
			switch {
			case d.Allow && (d.Always || d.OptionID == "acceptForSession") && always != "":
				return map[string]any{"decision": always}
			case d.Allow:
				return map[string]any{"decision": yes}
			}
			return map[string]any{"decision": no}
		}
	}
	switch method {
	case "item/commandExecution/requestApproval":
		kind := harness.ToolShell
		if p["networkApprovalContext"] != nil {
			kind = harness.ToolFetch // a network-access prompt
		}
		ask(harness.ToolRequest{CallID: env.ItemID, Name: "commandExecution", Kind: kind, Command: commandString(p["command"]), Input: p},
			codexOptions, decision("accept", "acceptForSession", "decline"))
	case "item/fileChange/requestApproval":
		s.mu.Lock()
		paths := append([]string(nil), s.files[env.ItemID]...)
		s.mu.Unlock()
		if root, ok := p["grantRoot"].(string); ok && root != "" {
			paths = append(paths, root)
		}
		ask(harness.ToolRequest{CallID: env.ItemID, Name: "fileChange", Kind: harness.ToolWrite, Paths: paths, Input: p},
			codexOptions, decision("accept", "acceptForSession", "decline"))
	case "item/permissions/requestApproval":
		ask(harness.ToolRequest{CallID: env.ItemID, Name: "requestPermissions", Kind: harness.ToolOther, Input: p}, nil,
			func(d harness.Decision) any {
				if d.Allow {
					return map[string]any{"permissions": p["permissions"], "scope": "turn"}
				}
				return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
			})
	case "execCommandApproval": // legacy v1 shape
		ask(harness.ToolRequest{Name: "execCommand", Kind: harness.ToolShell, Command: commandString(p["command"]), Input: p}, nil,
			decision("approved", "approved_for_session", "denied"))
	case "applyPatchApproval": // legacy v1 shape
		ask(harness.ToolRequest{Name: "applyPatch", Kind: harness.ToolWrite, Input: p}, nil,
			decision("approved", "approved_for_session", "denied"))
	case "item/tool/requestUserInput":
		ev := e
		ev.ID, ev.Questions = "req-"+firstNonEmpty(env.ItemID, method), parseQuestions(p)
		s.em.AskQuestions(tctx, ev, func(ans harness.Answers) {
			out := map[string]any{}
			for id, a := range ans {
				out[id] = map[string]any{"answers": a}
			}
			reply(map[string]any{"answers": out}, nil)
		})
	case "mcpServer/elicitation/request":
		reply(map[string]any{"action": "decline", "content": nil}, nil)
	default:
		reply(nil, &harness.RPCError{Code: -32601, Message: "Plexus does not handle " + method})
	}
}

func parseQuestions(p map[string]any) []harness.Question {
	var out []harness.Question
	list, _ := p["questions"].([]any)
	for _, x := range list {
		q, _ := x.(map[string]any)
		id, _ := q["id"].(string)
		text, _ := q["question"].(string)
		hdr, _ := q["header"].(string)
		other, _ := q["isOther"].(bool)
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
		out = append(out, harness.Question{ID: id, Header: hdr, Text: text, Options: opts, FreeText: other || len(opts) == 0})
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

// lookup prefers the native codex.exe; under npm it lives in a vendor dir
// (which of the two depends on the version: unverified).
var lookup = harness.Lookup{Names: []string{"codex"}, Npm: []string{
	"@openai/codex/node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe",
	"@openai/codex/vendor/x86_64-pc-windows-msvc/bin/codex.exe",
	"@openai/codex/bin/codex.js"}}
