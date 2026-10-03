// Package acp is a generic Agent Client Protocol (agentclientprotocol.com)
// client for long-tail harnesses. Any ACP-speaking harness plugs in with a
// Config value only (Gemini CLI is registered below; more can be added from
// config.json without writing Go). ACP is a lowest common denominator: for
// harnesses with a richer native protocol write a native adapter instead
// (DSH has one: internal/adapters/dsh; "dsh_acp" is only a fallback).
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

// Config describes one ACP harness.
type Config struct {
	Name        string   `json:"name"`         // registry name, e.g. "dsh"
	Executables []string `json:"executables"`  // names looked up on PATH / known dirs
	Args        []string `json:"args"`         // args that start ACP mode on stdio
	VersionArgs []string `json:"version_args"` // default ["--version"]
	CredFiles   []string `json:"cred_files"`   // login evidence, relative to the home dir
	CredEnv     []string `json:"cred_env"`     // login evidence env var names
	ExeEnv      string   `json:"exe_env"`      // env var that overrides the executable path
	// Npm lists entry points inside the npm global node_modules (native exe
	// or .js run with node), preferred over the package's .cmd shim.
	Npm []string `json:"npm,omitempty"`
}

func init() {
	// Commands below follow the vendors' published ACP entry points; they
	// have not been verified against a real install on Windows yet.
	harness.Register(New(Config{Name: "dsh_acp", Executables: []string{"dsh"}, Args: []string{"--profile", "acp"},
		Npm: []string{"@deepseek-ai/dsh/lib/bin.js"}, CredFiles: []string{".dsh/.credentials.yaml", ".dsh/.env"},
		CredEnv: []string{"DEEPSEEK_API_KEY"}, ExeEnv: "PLEXUS_DSH_EXE"})) // fallback only; the native dsh adapter is preferred
	harness.Register(New(Config{Name: "gemini_cli", Executables: []string{"gemini"}, Args: []string{"--experimental-acp"},
		Npm:       []string{"@google/gemini-cli/dist/index.js"},
		CredFiles: []string{".gemini/oauth_creds.json"}, CredEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		ExeEnv: "PLEXUS_GEMINI_EXE"}))
}

// Adapter is an ACP harness built from a Config.
type Adapter struct{ cfg Config }

// New builds an adapter from cfg.
func New(cfg Config) *Adapter {
	if len(cfg.VersionArgs) == 0 {
		cfg.VersionArgs = []string{"--version"}
	}
	return &Adapter{cfg: cfg}
}

func (a *Adapter) Name() string { return a.cfg.Name }

func (a *Adapter) Capabilities() harness.Capabilities {
	n, e, u := harness.Native, harness.Emulated, harness.Unsupported
	return harness.Capabilities{PermissionCallback: n, AskUser: u, BackgroundTasks: u, Subagents: u,
		SlashCommands: n, Effort: u, Resume: n, Interrupt: n, StopHook: u, SystemPrompt: e, Control: n,
		PerTaskStop: u, HostTools: u, GuestLock: u}
}

func (a *Adapter) Detect(ctx context.Context, env harness.Env) harness.DetectionResult {
	r := harness.DetectionResult{Harness: a.cfg.Name, Caps: a.Capabilities(), LoggedIn: harness.LoginUnknown}
	override := ""
	if a.cfg.ExeEnv != "" {
		override = env.Getenv(a.cfg.ExeEnv)
	}
	r.Path = harness.FindExecutable(env, override, harness.Lookup{Names: a.cfg.Executables, Npm: a.cfg.Npm})
	if r.Path == "" {
		r.LoggedIn = harness.LoginNo
		return r
	}
	r.Installed = true
	if v, err := env.RunVersion(ctx, r.Path, a.cfg.VersionArgs...); err == nil {
		r.Version = harness.FirstLine(v)
	} else {
		r.Error = "version check failed"
	}
	for _, f := range a.cfg.CredFiles {
		if env.Exists(filepath.Join(env.Home, filepath.FromSlash(f))) {
			r.LoggedIn = harness.LoginYes
			r.Evidence = append(r.Evidence, "file:"+f)
		}
	}
	for _, k := range a.cfg.CredEnv {
		if env.Getenv(k) != "" {
			r.LoggedIn = harness.LoginYes
			r.Evidence = append(r.Evidence, "env:"+k)
		}
	}
	return r
}

func (a *Adapter) StartSession(ctx context.Context, o harness.SessionOptions) (harness.Session, error) {
	exe := o.Exe
	if exe == "" {
		exe = a.Detect(ctx, harness.OSEnv()).Path
	}
	p, err := harness.StartProc(ctx, exe, append(append([]string{}, a.cfg.Args...), o.Args...), o.Workdir, o.Env)
	if err != nil {
		return nil, err
	}
	s := &session{p: p, rpc: harness.NewRPC(p, true), em: harness.NewEmitter(), workdir: o.Workdir, persona: o.Persona}
	s.rpc.OnNotify = s.notify
	s.rpc.OnRequest = s.request
	go func() { s.rpc.Run(); s.exited() }()
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fail := func(err error) (harness.Session, error) { s.Close(); return nil, err }
	var init struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Resume *struct{} `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := s.rpc.Call(cctx, "initialize", map[string]any{"protocolVersion": 1,
		// fs and terminal stay false so the agent keeps its own native file
		// and shell tools (advertising them would reroute its I/O to us).
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]any{"name": "plexus", "title": "Plexus", "version": "0.1.0"}}, &init); err != nil {
		return fail(fmt.Errorf("%s initialize: %w", a.cfg.Name, err))
	}
	if o.ResumeID != "" && init.AgentCapabilities.SessionCapabilities.Resume != nil {
		// session/resume reconnects without replaying history.
		err := s.rpc.Call(cctx, "session/resume", map[string]any{"sessionId": o.ResumeID, "cwd": o.Workdir, "mcpServers": []any{}}, nil)
		if err == nil {
			s.id = o.ResumeID
			return s, nil
		}
	}
	if o.ResumeID != "" && init.AgentCapabilities.LoadSession {
		// History replayed by session/load arrives while no turn is active and is ignored.
		err := s.rpc.Call(cctx, "session/load", map[string]any{"sessionId": o.ResumeID, "cwd": o.Workdir, "mcpServers": []any{}}, nil)
		if err == nil {
			s.id = o.ResumeID
			return s, nil
		}
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.rpc.Call(cctx, "session/new", map[string]any{"cwd": o.Workdir, "mcpServers": []any{}}, &res); err != nil {
		var re *harness.RPCError
		if errors.As(err, &re) && strings.Contains(strings.ToLower(re.Message), "auth") {
			return fail(fmt.Errorf("%s is not logged in: %w", a.cfg.Name, err))
		}
		return fail(fmt.Errorf("%s session/new: %w", a.cfg.Name, err))
	}
	s.id = res.SessionID
	return s, nil
}

type session struct {
	p       *harness.Proc
	rpc     *harness.RPC
	em      *harness.Emitter
	turn    harness.TurnState
	workdir string
	persona string
	id      string

	mu     sync.Mutex
	text   strings.Builder
	primed bool
}

func (s *session) ID() string                   { return s.id }
func (s *session) Events() <-chan harness.Event { return s.em.C() }

func (s *session) Close() error {
	err := s.p.Close()
	s.em.Close()
	return err
}

func (s *session) exited() {
	if id, _ := s.turn.Current(); s.turn.End(id) {
		s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "harness exited"})
	}
	s.em.Emit(harness.Event{Kind: harness.EventError, Text: "harness exited"})
	s.em.Close()
}

func (s *session) Send(ctx context.Context, t harness.Turn) (string, error) {
	id, tctx, ok := s.turn.Begin(ctx)
	if !ok {
		return "", errors.New("a turn is already running")
	}
	s.mu.Lock()
	s.text.Reset()
	text := t.Text
	if !s.primed && s.persona != "" {
		// ACP has no system-prompt field; send the persona once as context.
		text = s.persona + "\n\n---\n\n" + text
	}
	s.primed = true
	s.mu.Unlock()
	go func() {
		var res struct {
			StopReason string `json:"stopReason"`
		}
		// session/prompt returns when the turn ends. Use a context that the
		// turn's own end does not cancel, so we can tell the two apart.
		err := s.rpc.Call(context.Background(), "session/prompt", map[string]any{"sessionId": s.id,
			"prompt": []any{map[string]any{"type": "text", "text": text}}}, &res)
		if !s.turn.End(id) {
			return // already ended as cancelled
		}
		s.mu.Lock()
		full := s.text.String()
		s.mu.Unlock()
		e := harness.Event{TurnID: id, SessionID: s.id}
		switch {
		case err != nil:
			e.Kind, e.Text = harness.EventError, err.Error()
		case res.StopReason == "end_turn" || res.StopReason == "":
			s.em.Emit(harness.Event{TurnID: id, SessionID: s.id, Kind: harness.EventMessage, Text: full})
			e.Kind, e.Text = harness.EventFinal, full
		default:
			e.Kind, e.Text, e.Status = harness.EventError, "stopped: "+res.StopReason, res.StopReason
		}
		s.em.Emit(e)
	}()
	go func() {
		<-tctx.Done()
		if s.turn.End(id) {
			_ = s.rpc.Notify("session/cancel", map[string]any{"sessionId": s.id})
			s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "turn cancelled", SessionID: s.id})
		}
	}()
	return id, nil
}

func (s *session) Interrupt(ctx context.Context) error {
	return s.rpc.Notify("session/cancel", map[string]any{"sessionId": s.id})
}

// Control calls any ACP method (e.g. "session/set_mode"); sessionId is
// added when the payload omits it.
func (s *session) Control(ctx context.Context, name string, payload json.RawMessage) (json.RawMessage, error) {
	params := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &params); err != nil {
			return nil, err
		}
	}
	if _, ok := params["sessionId"]; !ok {
		params["sessionId"] = s.id
	}
	var out json.RawMessage
	err := s.rpc.Call(ctx, name, params, &out)
	return out, err
}

type toolCall struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
	RawInput any `json:"rawInput"`
}

func (tc toolCall) request() harness.ToolRequest {
	r := harness.ToolRequest{Name: firstNonEmpty(tc.Title, tc.Kind), Input: tc.RawInput}
	for _, l := range tc.Locations {
		r.Paths = append(r.Paths, l.Path)
	}
	switch tc.Kind {
	case "read", "search":
		r.Kind = harness.ToolRead
		if len(r.Paths) == 0 {
			r.Paths = []string{"."}
		}
	case "edit", "delete", "move":
		r.Kind = harness.ToolWrite
	case "execute":
		r.Kind = harness.ToolShell
		if m, ok := tc.RawInput.(map[string]any); ok {
			r.Command, _ = m["command"].(string)
		}
	case "fetch":
		r.Kind = harness.ToolFetch
	case "think":
		r.Kind = harness.ToolMeta
	default:
		r.Kind = harness.ToolOther
	}
	return r
}

func (s *session) notify(method string, raw json.RawMessage) {
	if method != "session/update" {
		return
	}
	var p struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Status string `json:"status"`
			toolCall
		} `json:"update"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	turnID, _ := s.turn.Current()
	if turnID == "" {
		return // e.g. history replayed by session/load
	}
	e := harness.Event{TurnID: turnID, SessionID: s.id, Raw: raw}
	u := p.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if u.Content.Type != "text" {
			return
		}
		s.mu.Lock()
		s.text.WriteString(u.Content.Text)
		s.mu.Unlock()
		e.Kind, e.Text = harness.EventTextDelta, u.Content.Text
	case "tool_call":
		r := u.toolCall.request()
		e.Kind, e.ID, e.Tool, e.Status = harness.EventToolUse, u.ToolCallID, &r, u.Status
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		e.Kind, e.ID, e.Status = harness.EventToolResult, u.ToolCallID+":result", u.Status
	case "user_message_chunk":
		return
	default:
		e.Kind, e.Name = harness.EventExtension, "session/update/"+u.SessionUpdate
	}
	s.em.Emit(e)
}

func (s *session) request(method string, raw json.RawMessage, reply func(any, *harness.RPCError)) {
	turnID, tctx := s.turn.Current()
	base := harness.Event{TurnID: turnID, SessionID: s.id, Raw: raw}
	deny := func(msg string) { reply(nil, &harness.RPCError{Code: -32000, Message: "Plexus policy: " + msg}) }
	switch method {
	case "session/request_permission":
		var p struct {
			ToolCall toolCall `json:"toolCall"`
			Options  []struct {
				OptionID string `json:"optionId"`
				Name     string `json:"name"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(raw, &p)
		var opts []harness.PermissionOption
		for _, o := range p.Options {
			opts = append(opts, harness.PermissionOption{ID: o.OptionID, Label: o.Name, Kind: harness.OptionKind(o.Kind)})
		}
		pick := func(kinds ...harness.OptionKind) string {
			for _, k := range kinds {
				for _, o := range opts {
					if o.Kind == k {
						return o.ID
					}
				}
			}
			return ""
		}
		e := base
		e.ID = "req-" + p.ToolCall.ToolCallID
		e.Perm = &harness.PermissionRequest{Tool: p.ToolCall.request(), Options: opts}
		s.em.Ask(tctx, e, func(d harness.Decision) {
			id := pick(harness.RejectOnce, harness.RejectAlways)
			if d.Allow {
				id = pick(harness.AllowOnce, harness.AllowAlways)
				if d.Always {
					id = firstNonEmpty(pick(harness.AllowAlways), id)
				}
				if d.OptionID != "" {
					id = d.OptionID
				}
			}
			if id == "" {
				reply(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil)
				return
			}
			reply(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}, nil)
		})
	case "fs/read_text_file":
		var p struct {
			Path  string `json:"path"`
			Line  int    `json:"line"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal(raw, &p)
		e := base
		e.Perm = &harness.PermissionRequest{Tool: harness.ToolRequest{Name: method, Kind: harness.ToolRead, Paths: []string{p.Path}}}
		s.em.Ask(tctx, e, func(d harness.Decision) {
			if !d.Allow {
				deny(d.Reason)
				return
			}
			b, err := os.ReadFile(p.Path)
			if err != nil {
				reply(nil, &harness.RPCError{Code: -32002, Message: "cannot read file"})
				return
			}
			reply(map[string]any{"content": sliceLines(string(b), p.Line, p.Limit)}, nil)
		})
	case "fs/write_text_file":
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(raw, &p)
		e := base
		e.Perm = &harness.PermissionRequest{Tool: harness.ToolRequest{Name: method, Kind: harness.ToolWrite, Paths: []string{p.Path}}}
		s.em.Ask(tctx, e, func(d harness.Decision) {
			if !d.Allow {
				deny(d.Reason)
				return
			}
			if err := os.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil {
				reply(nil, &harness.RPCError{Code: -32002, Message: "cannot write file"})
				return
			}
			reply(nil, nil)
		})
	default:
		reply(nil, &harness.RPCError{Code: -32601, Message: "Plexus does not handle " + method})
	}
}

// sliceLines returns limit lines starting at 1-based line (0 = all).
func sliceLines(s string, line, limit int) string {
	if line <= 0 && limit <= 0 {
		return s
	}
	ls := strings.SplitAfter(s, "\n")
	start := 0
	if line > 0 {
		start = line - 1
	}
	if start > len(ls) {
		return ""
	}
	end := len(ls)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return strings.Join(ls[start:end], "")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
