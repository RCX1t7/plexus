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
// The bridge plugin is embedded (bridge/plexus-bridge.min.mjs) and installed
// into the plexus profile on start. See docs/DSH_BRIDGE.md for the protocol.
//
//	client -> dsh  plexus.initialize   {protocol, client}            -> {protocol, capabilities}
//	client -> dsh  plexus.session.open {cwd, persona, resume, tools, model?, effort?} -> {sessionId}
//	client -> dsh  plexus.prompt       {sessionId, text, guest}      -> {turnId}
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
	"strings"
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
		PerTaskStop: n, HostTools: n, GuestLock: n} // via the bridge plugin: UNVERIFIED
}

// Detect finds DSH's JS entry (never a .cmd shim, never DSH Desktop's
// copy), checks Node.js (>= MinNode), runs `node bin.js --version`, and
// checks DSH's credential sources (env, %DSH_HOME%\.credentials.yaml,
// %DSH_HOME%\.env) for existence only. Plexus never reads or passes the key:
// DSH uses its own login. A key in <cwd>\.env is not looked at, so
// "unknown" is reported rather than "no" when nothing is found.
func (a Adapter) Detect(ctx context.Context, env harness.Env) harness.DetectionResult {
	r := harness.DetectionResult{Harness: a.Name(), Caps: a.Capabilities(), LoggedIn: harness.LoginUnknown}
	p, why := find(env)
	if p == "" {
		r.LoggedIn = harness.LoginNo
		r.Error = why
		return r
	}
	r.Path, r.Installed = p, true
	exe, args := p, []string{"--version"}
	if isJS(p) {
		node, err := findNode(env, p)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		nv, err := checkNode(ctx, env, node)
		r.Evidence = append(r.Evidence, "node:"+nv)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		exe, args = node, []string{p, "--version"}
	}
	if v, err := env.RunVersion(ctx, exe, args...); err == nil {
		r.Version = harness.FirstLine(v)
	} else {
		r.Error = "version check failed"
	}
	if env.Getenv("DEEPSEEK_API_KEY") != "" {
		r.LoggedIn = harness.LoginYes
		r.Evidence = append(r.Evidence, "env:DEEPSEEK_API_KEY")
	}
	home := DSHHome(env)
	for _, f := range []string{".credentials.yaml", ".env"} {
		if env.Exists(filepath.Join(home, f)) {
			r.LoggedIn = harness.LoginYes
			r.Evidence = append(r.Evidence, "file:.dsh/"+f)
		}
	}
	if !Bundled() {
		r.Error = firstNonEmpty(r.Error, "this build does not include the DSH bridge plugin yet")
	} else if env.Exists(filepath.Join(home, "profiles", ProfileName, pluginFile)) {
		r.Evidence = append(r.Evidence, "file:.dsh/profiles/"+ProfileName+"/"+pluginFile)
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a Adapter) StartSession(ctx context.Context, o harness.SessionOptions) (harness.Session, error) {
	env := harness.OSEnv()
	exe := o.Exe
	if exe == "" {
		var why string
		if exe, why = find(env); exe == "" {
			return nil, errors.New(firstNonEmpty(why, "dsh not found"))
		}
	}
	home := DSHHome(env)
	args := []string{"--profile", ProfileName}
	if Bundled() {
		r, err := InstallAt(home) // idempotent refresh of the plexus profile
		if err != nil {
			return nil, fmt.Errorf("install the DSH bridge plugin: %w", err)
		}
		if env.Exists(r.PatchPath) {
			args = append(args, "--patch", r.PatchPath)
		}
	}
	p, err := harness.StartProc(ctx, exe, append(args, o.Args...), o.Workdir, o.Env)
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
		Protocol     int               `json:"protocol"`
		Capabilities map[string]string `json:"capabilities"`
	}
	if err := s.rpc.Call(cctx, "plexus.initialize", map[string]any{"protocol": Protocol,
		"client": map[string]any{"name": "plexus", "version": "0.1.0"}}, &init); err != nil {
		return fail(fmt.Errorf("dsh bridge initialize (is the plexus profile installed?): %w", err))
	}
	if init.Protocol != Protocol {
		return fail(fmt.Errorf("dsh bridge speaks protocol %d, want %d", init.Protocol, Protocol))
	}
	// Start-up rule (CR-6): DSH accepts strangers, so its guest lock must be
	// native (the plugin's tools.guard). Fail closed: refuse the whole session
	// unless the plugin affirmatively reports guest_lock=native. A missing or
	// empty capability means the plugin could not install the guard, so a
	// stranger's turn could reach write/exec tools -- that is exactly the case
	// we must refuse, not wave through. The guest lock is never waived by
	// ungated_ok (that only waives the dangerous-action gate, decided core-side
	// with the bot config).
	if gl := init.Capabilities["guest_lock"]; gl != "native" {
		return fail(fmt.Errorf("dsh bridge guest lock is not native (guest_lock=%q); DSH accepts strangers, "+
			"so Plexus refuses to start a session it cannot lock", gl))
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	tools := o.HostTools
	if tools == nil {
		tools = []harness.ToolSpec{}
	}
	open := map[string]any{"cwd": o.Workdir, "persona": o.Persona, "resume": o.ResumeID, "tools": tools}
	// Model/Effort are optional: omitted (not "") when empty, so the bridge
	// resolves DSH's own configured default (agent-default-model).
	if m := strings.TrimSpace(o.Model); m != "" {
		open["model"] = m
	}
	if e := strings.TrimSpace(o.Effort); e != "" {
		open["effort"] = e
	}
	if err := s.rpc.Call(cctx, "plexus.session.open", open, &res); err != nil {
		if o.ResumeID != "" && isActiveElsewhere(err) {
			return fail(fmt.Errorf("%w: %v", harness.ErrActiveElsewhere, err))
		}
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
	// Ask the bridge to exit cleanly (plexus.shutdown) before killing the
	// process tree, so DSH can flush the session. Best effort and bounded:
	// p.Close() still kills the tree if the bridge does not exit in time.
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.rpc.Call(sctx, "plexus.shutdown", map[string]any{}, nil)
	cancel()
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
	// plexus.prompt runs on the caller's ctx, not the turn's (review #13, same
	// as the Codex fix b6d2ee7): the bridge streams events ahead of its reply,
	// so the turn can reach its final (and End cancel tctx) before the reply
	// arrives; that must not turn a finished turn into "context canceled".
	// Guest is the only authority flag on the wire: a guest turn is locked
	// natively by the bridge (only plexus_post); there is no level.
	if err := s.rpc.Call(ctx, "plexus.prompt", map[string]any{"sessionId": s.id, "text": t.Text, "guest": t.Guest}, &res); err != nil {
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
		// one classifier for all adapters: the Go core reads Kind/Command/Paths.
		// DSH-internal meta tools (send_message, subagent*, job_*, ...) are
		// pinned to ToolMeta so the classifier never mistakes them for an
		// egress or a dangerous call (CR-5); Normalize keeps a preset kind.
		e.Perm = &harness.PermissionRequest{Tool: mapTool(w.Tool), Reason: w.Reason, Options: w.Options}
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

// lookup: DSH ships no native exe; run its JS entry with node, never a
// dsh.cmd (npm's or DSH Desktop's).
var lookup = harness.Lookup{Names: []string{"dsh"}, Npm: []string{"@deepseek-ai/dsh/lib/bin.js"}}

// isActiveElsewhere maps the bridge's session-lease error (one live writer
// per DSH session) to "open in another DSH: DSH Desktop or a CLI".
func isActiveElsewhere(err error) bool {
	l := strings.ToLower(err.Error())
	for _, k := range []string{"lease", "locked", "active writer", "in use", "another process", "already open"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}
