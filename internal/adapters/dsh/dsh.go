// Package dsh is the native DeepSeek Harness adapter.
//
// ACP strips DSH's native features and `dsh --profile sdk` streams events
// but cannot approve or cancel, so Plexus runs DSH with its own profile:
//
//	dsh --profile plexus
//
// where the "plexus" profile (created from the sdk template) loads a small
// bridge plugin speaking the Plexus bridge protocol: JSON-RPC 2.0 over
// stdio whose payloads mirror the normalized harness event model 1:1.
//
// STATUS: UNVERIFIED. The Go side below is complete and tested against a
// fake; the DSH-side plugin (plugins/dsh-bridge, TypeScript) is not written
// yet. See docs/DSH_BRIDGE.md for the protocol.
//
//	client -> dsh  plexus.initialize   {protocol, client}            -> {protocol, capabilities}
//	client -> dsh  plexus.session.open {cwd, persona, resume, tools} -> {sessionId}
//	client -> dsh  plexus.prompt       {sessionId, text, level}      -> {turnId}
//	client -> dsh  plexus.cancel       {sessionId}                   -> {}
//	client -> dsh  plexus.steer        {sessionId, text}             -> {}
//	client -> dsh  plexus.stopTask     {sessionId, taskId}           -> {}
//	client -> dsh  plexus.control      {sessionId, name, payload}    -> any
//	dsh -> client  plexus.event        (notification) harness.Event fields
//	dsh -> client  plexus.permission   {turnId,id,parentId,tool,reason,options} -> {allow,optionId,always,reason}
//	dsh -> client  plexus.question     {turnId,id,parentId,questions}           -> {answers}
//	dsh -> client  plexus.tool         {turnId,id,name,arguments}               -> {text,isError}
package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

// Protocol is the bridge protocol version this adapter speaks.
const Protocol = 1

func init() { harness.Register(Adapter{}) }

// Adapter is the native DSH harness.
type Adapter struct{}

func (Adapter) Name() string { return "dsh" }

func (Adapter) Capabilities() harness.Capabilities {
	n, u := harness.Native, harness.Unsupported
	return harness.Capabilities{PermissionCallback: n, AskUser: n, BackgroundTasks: n, Subagents: n,
		SlashCommands: u, Effort: u, Resume: n, Interrupt: n, StopHook: u, SystemPrompt: n, Control: n,
		PerTaskStop: n, HostTools: n} // via the bridge plugin: UNVERIFIED
}

// Detect runs `dsh --version` and checks DSH's documented credential
// sources (env, %DSH_HOME%\.credentials.yaml, %DSH_HOME%\.env) for
// existence only. A key in <cwd>\.env is not looked at, so "unknown" is
// reported rather than "no" when nothing is found.
func (a Adapter) Detect(ctx context.Context, env harness.Env) harness.DetectionResult {
	r := harness.DetectionResult{Harness: a.Name(), Caps: a.Capabilities(), LoggedIn: harness.LoginUnknown}
	r.Path = harness.FindExecutable(env, env.Getenv("PLEXUS_DSH_EXE"), lookup)
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
	if env.Getenv("DEEPSEEK_API_KEY") != "" {
		r.LoggedIn = harness.LoginYes
		r.Evidence = append(r.Evidence, "env:DEEPSEEK_API_KEY")
	}
	home := env.Getenv("DSH_HOME")
	if home == "" {
		home = filepath.Join(env.Home, ".dsh")
	}
	for _, f := range []string{".credentials.yaml", ".env"} {
		if env.Exists(filepath.Join(home, f)) {
			r.LoggedIn = harness.LoginYes
			r.Evidence = append(r.Evidence, "file:.dsh/"+f)
		}
	}
	return r
}

func (a Adapter) StartSession(ctx context.Context, o harness.SessionOptions) (harness.Session, error) {
	exe := o.Exe
	if exe == "" {
		exe = a.Detect(ctx, harness.OSEnv()).Path
	}
	p, err := harness.StartProc(ctx, exe, append([]string{"--profile", "plexus"}, o.Args...), o.Workdir, o.Env)
	if err != nil {
		return nil, err
	}
	s := &session{p: p, rpc: harness.NewRPC(p, true), em: harness.NewEmitter(), tasks: map[string]bool{}}
	s.rpc.OnNotify = s.notify
	s.rpc.OnRequest = s.request
	go func() { s.rpc.Run(); s.exited() }()
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fail := func(err error) (harness.Session, error) { s.Close(); return nil, err }
	var init struct {
		Protocol int `json:"protocol"`
	}
	if err := s.rpc.Call(cctx, "plexus.initialize", map[string]any{"protocol": Protocol,
		"client": map[string]any{"name": "plexus", "version": "0.1.0"}}, &init); err != nil {
		return fail(fmt.Errorf("dsh bridge initialize (is the plexus profile installed?): %w", err))
	}
	if init.Protocol != Protocol {
		return fail(fmt.Errorf("dsh bridge speaks protocol %d, want %d", init.Protocol, Protocol))
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	tools := o.HostTools
	if tools == nil {
		tools = []harness.ToolSpec{}
	}
	if err := s.rpc.Call(cctx, "plexus.session.open", map[string]any{"cwd": o.Workdir,
		"persona": o.Persona, "resume": o.ResumeID, "tools": tools}, &res); err != nil {
		return fail(fmt.Errorf("dsh session open: %w", err))
	}
	s.id = res.SessionID
	return s, nil
}

type session struct {
	p    *harness.Proc
	rpc  *harness.RPC
	em   *harness.Emitter
	turn harness.TurnState
	id   string

	mu     sync.Mutex
	native string // bridge turn id of the running turn
	plexus string // Plexus turn id
	tasks  map[string]bool
}

// Steer folds text into the running turn.
func (s *session) Steer(ctx context.Context, text string) error {
	if cur, _ := s.turn.Current(); cur == "" {
		return errors.New("no turn running")
	}
	return s.rpc.Call(ctx, "plexus.steer", map[string]any{"sessionId": s.id, "text": text}, nil)
}

// BackgroundTasks lists background jobs the bridge reported as running.
func (s *session) BackgroundTasks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.tasks {
		out = append(out, id)
	}
	return out
}

// StopTask stops one background job.
func (s *session) StopTask(ctx context.Context, id string) error {
	return s.rpc.Call(ctx, "plexus.stopTask", map[string]any{"sessionId": s.id, "taskId": id}, nil)
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
		s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "dsh exited"})
	}
	s.em.Emit(harness.Event{Kind: harness.EventError, Text: "dsh exited"})
	s.em.Close()
}

func (s *session) Send(ctx context.Context, t harness.Turn) (string, error) {
	id, tctx, ok := s.turn.Begin(ctx)
	if !ok {
		return "", errors.New("a turn is already running")
	}
	s.mu.Lock()
	s.native, s.plexus = "", id
	s.mu.Unlock()
	var res struct {
		TurnID string `json:"turnId"`
	}
	if err := s.rpc.Call(tctx, "plexus.prompt", map[string]any{"sessionId": s.id, "text": t.Text, "level": t.Level.String()}, &res); err != nil {
		s.turn.End(id)
		return "", err
	}
	s.mu.Lock()
	if s.native == "" {
		s.native = res.TurnID
	}
	s.mu.Unlock()
	go func() {
		<-tctx.Done()
		if s.turn.End(id) {
			ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Interrupt(ictx)
			cancel()
			s.em.Emit(harness.Event{Kind: harness.EventError, TurnID: id, Text: "turn cancelled", SessionID: s.id})
		}
	}()
	return id, nil
}

func (s *session) Interrupt(ctx context.Context) error {
	return s.rpc.Call(ctx, "plexus.cancel", map[string]any{"sessionId": s.id}, nil)
}

func (s *session) Control(ctx context.Context, name string, payload json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	err := s.rpc.Call(ctx, "plexus.control", map[string]any{"sessionId": s.id, "name": name, "payload": payload}, &out)
	return out, err
}

// wireEvent is the bridge's notification payload: harness.Event fields with
// the bridge's own turn id.
type wireEvent struct {
	harness.Event
	BridgeTurn string `json:"turnId"`
}

// mapTurn converts a bridge turn id to the Plexus turn id ("" = no turn).
func (s *session) mapTurn(bridge string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, _ := s.turn.Current()
	if bridge != "" && s.native == "" && cur != "" && cur == s.plexus {
		s.native = bridge // an event raced ahead of the plexus.prompt reply
	}
	if bridge != "" && bridge == s.native && cur == s.plexus {
		return s.plexus
	}
	return ""
}

func (s *session) notify(method string, raw json.RawMessage) {
	if method != "plexus.event" {
		return
	}
	var w wireEvent
	if json.Unmarshal(raw, &w) != nil {
		return
	}
	e := w.Event
	e.TurnID, e.SessionID, e.Raw = s.mapTurn(w.BridgeTurn), s.id, raw
	if e.Kind == harness.EventBackground && e.Name != "" {
		s.mu.Lock()
		switch e.Status {
		case "started", "running":
			s.tasks[e.Name] = true
		default:
			delete(s.tasks, e.Name)
		}
		s.mu.Unlock()
	}
	if e.Kind == harness.EventFinal || (e.Kind == harness.EventError && e.TurnID != "") {
		if !s.turn.End(e.TurnID) {
			return
		}
	}
	s.em.Emit(e)
}

func (s *session) request(method string, raw json.RawMessage, reply func(any, *harness.RPCError)) {
	var w struct {
		TurnID    string                     `json:"turnId"`
		ID        string                     `json:"id"`
		ParentID  string                     `json:"parentId"`
		Tool      harness.ToolRequest        `json:"tool"`
		Reason    string                     `json:"reason"`
		Options   []harness.PermissionOption `json:"options"`
		Questions []harness.Question         `json:"questions"`
		Name      string                     `json:"name"`
		Arguments any                        `json:"arguments"`
	}
	_ = json.Unmarshal(raw, &w)
	_, tctx := s.turn.Current()
	e := harness.Event{ID: w.ID, ParentID: w.ParentID, TurnID: s.mapTurn(w.TurnID), SessionID: s.id, Raw: raw}
	switch method {
	case "plexus.permission":
		e.Perm = &harness.PermissionRequest{Tool: w.Tool, Reason: w.Reason, Options: w.Options}
		s.em.Ask(tctx, e, func(d harness.Decision) {
			reply(map[string]any{"allow": d.Allow, "optionId": d.OptionID, "always": d.Always, "reason": d.Reason}, nil)
		})
	case "plexus.question":
		e.Questions = w.Questions
		s.em.AskQuestions(tctx, e, func(a harness.Answers) { reply(map[string]any{"answers": a}, nil) })
	case "plexus.tool":
		e.Name = w.Name
		e.Tool = &harness.ToolRequest{CallID: w.ID, Name: w.Name, Kind: harness.ToolMeta, Input: w.Arguments}
		s.em.CallTool(tctx, e, func(r harness.HostResult) { reply(map[string]any{"text": r.Text, "isError": r.IsError}, nil) })
	default:
		reply(nil, &harness.RPCError{Code: -32601, Message: "Plexus does not handle " + method})
	}
}

// lookup: DSH ships no native exe; run its JS entry with node, not dsh.cmd.
var lookup = harness.Lookup{Names: []string{"dsh"}, Npm: []string{"@deepseek-ai/dsh/lib/bin.js"}}
