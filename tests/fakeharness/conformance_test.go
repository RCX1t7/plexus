package fakeharness_test

// Conformance self-tests of the fake harness: every native protocol mode is
// driven through internal/hclient (hub-side shapes mirroring the Plexus
// skeleton adapters). These test the fakes only; they are not a Plexus test.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/tests/fakeharness"
	"github.com/RCX1t7/plexus/tests/internal/hclient"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "fh-bin")
	fakeBin = filepath.Join(dir, "fakeharness")
	out, err := exec.Command("go", "build", "-o", fakeBin, "github.com/RCX1t7/plexus/tests/cmd/fakeharness").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build fakeharness: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var protos = []string{hclient.Claude, hclient.Codex, hclient.DSH, hclient.ACP}

const peers = "claude=U0CLAUDE,codex=U0CODEX,dsh=U0DSH,acp=U0ACP"

func frame(user, text string) string {
	return fmt.Sprintf("[Slack message from <@%s>, sim; access level: full]\n%s", user, text)
}

type env struct {
	t      *testing.T
	dir    string
	ledger string
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "input.txt"), []byte("harness hub\nclaude codex dsh\nslack\n"), 0o644)
	return &env{t: t, dir: dir, ledger: filepath.Join(dir, "ledger.jsonl")}
}

func (e *env) start(proto, role string, h hclient.Handler, mod func(*hclient.Options), extraEnv ...string) *hclient.Session {
	o := hclient.Options{Exe: fakeBin, Args: []string{"--sim-role", role}, Dir: e.dir, Setpgid: true,
		Env: append([]string{"HH_SIM_LEDGER=" + e.ledger, "HH_SIM_PEERS=" + peers, "HH_SIM_OWNER=U0SIN",
			"HH_SIM_STATE=" + filepath.Join(e.dir, "state"), "HH_SIM_TICK=100ms"}, extraEnv...)}
	if mod != nil {
		mod(&o)
	}
	s, err := hclient.Start(proto, o, h)
	if err != nil {
		e.t.Fatalf("%s start: %v", proto, err)
	}
	e.t.Cleanup(s.Kill)
	return s
}

func (e *env) entries() []fakeharness.LedgerEntry {
	f, err := os.Open(e.ledger)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []fakeharness.LedgerEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var le fakeharness.LedgerEntry
		if json.Unmarshal(sc.Bytes(), &le) == nil {
			out = append(out, le)
		}
	}
	return out
}

func (e *env) phases(action string) []string {
	var p []string
	for _, le := range e.entries() {
		if le.Action == action {
			p = append(p, le.Phase)
		}
	}
	return p
}

func (e *env) has(action, phase string) bool {
	for _, p := range e.phases(action) {
		if p == phase {
			return true
		}
	}
	return false
}

func prompt(t *testing.T, s *hclient.Session, text string) string {
	t.Helper()
	type r struct {
		f   string
		err error
	}
	ch := make(chan r, 1)
	go func() { f, err := s.Prompt(text, ""); ch <- r{f, err} }()
	select {
	case x := <-ch:
		if x.err != nil {
			t.Fatalf("prompt: %v", x.err)
		}
		return x.f
	case <-time.After(20 * time.Second):
		t.Fatal("prompt timeout")
	}
	return ""
}

func TestDetectProto(t *testing.T) {
	cases := map[string][]string{
		fakeharness.ProtoClaude: {"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		fakeharness.ProtoCodex:  {"app-server"},
		fakeharness.ProtoDSH:    {"--profile", "plexus"},
		fakeharness.ProtoACP:    {"--experimental-acp"},
	}
	for want, argv := range cases {
		if got := fakeharness.DetectProto(argv); got != want {
			t.Errorf("%v -> %q, want %q", argv, got, want)
		}
	}
}

// Ping + host-tool-free final text, permission allow and deny, on every protocol.
func TestProtocols_TurnAndPermission(t *testing.T) {
	for _, p := range protos {
		t.Run(p, func(t *testing.T) {
			e := newEnv(t)
			allow := true
			var asked []hclient.Tool
			var mu sync.Mutex
			s := e.start(p, "codex", hclient.Handler{Permission: func(tl hclient.Tool) bool {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, tl)
				return allow
			}}, nil)
			if f := prompt(t, s, frame("U0SIN", "<@U0CODEX> [sim:ping]")); f != fakeharness.TagOnline+" codex" {
				t.Fatalf("final %q", f)
			}
			if s.ID() == "" {
				t.Fatal("no session id (claude: from output messages; others: handshake)")
			}
			mu.Lock()
			allow = false
			mu.Unlock()
			prompt(t, s, frame("U0OTHER", "<@U0CODEX> [sim:member-write]"))
			if !e.has("X2", "denied") || e.has("X2", "started") {
				t.Fatalf("deny not honoured: %v", e.phases("X2"))
			}
			if _, err := os.Stat(filepath.Join(e.dir, "out", "member-codex.txt")); err == nil {
				t.Fatal("file written despite deny")
			}
			mu.Lock()
			allow = true
			mu.Unlock()
			prompt(t, s, frame("U0OTHER", "<@U0CODEX> [sim:member-write]"))
			if !e.has("X2", "completed") {
				t.Fatalf("allow not honoured: %v", e.phases("X2"))
			}
			mu.Lock()
			n := len(asked)
			mu.Unlock()
			if n < 2 {
				t.Fatalf("expected native permission requests, got %d", n)
			}
		})
	}
}

// The claude role's question + implementation + six-field handoff through host tools.
func TestProtocols_QuestionAndHandoff(t *testing.T) {
	for _, p := range []string{hclient.Claude, hclient.Codex, hclient.DSH} {
		t.Run(p, func(t *testing.T) {
			e := newEnv(t)
			var mu sync.Mutex
			calls := map[string][]map[string]any{}
			h := hclient.Handler{
				Question: func(q hclient.Question) (string, bool) {
					if !strings.Contains(q.Text, "lines/words/bytes") {
						t.Errorf("question %q", q.Text)
					}
					return "lines/words/bytes", true
				},
				HostTool: func(name string, args map[string]any) bool {
					mu.Lock()
					defer mu.Unlock()
					calls[name] = append(calls[name], args)
					return true
				},
			}
			s := e.start(p, "claude", h, func(o *hclient.Options) {
				o.HostTools = []string{"plexus_post", "plexus_ack", "plexus_delegate", "plexus_deliver", "plexus_review"}
			})
			if f := prompt(t, s, frame("U0SIN", "<@U0CLAUDE> <@U0CODEX> <@U0DSH> #目标 统计 input.txt")); !strings.Contains(f, fakeharness.TagSplit) {
				t.Fatalf("human-triggered turn must answer in final text, got %q", f)
			}
			if f := prompt(t, s, frame("U0CODEX", fakeharness.TagAgree+" codex 负责测试校验。")); f != "" {
				t.Fatalf("autonomous turn must stay quiet in final text (posts via host tools), got %q", f)
			}
			raw, _ := os.ReadFile(filepath.Join(e.dir, "out", "result.json"))
			if string(raw) != fakeharness.ResultJSON(fakeharness.CountBytes([]byte("harness hub\nclaude codex dsh\nslack\n")), true) {
				t.Fatalf("result.json %q", raw)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls["plexus_ack"]) != 1 || len(calls["plexus_delegate"]) != 1 {
				t.Fatalf("host tool calls %v", calls)
			}
			for _, f := range fakeharness.HandoffFields {
				if calls["plexus_delegate"][0][f] == nil {
					t.Errorf("handoff field %s missing", f)
				}
			}
		})
	}
}

// Without host tools the same autonomous turn falls back to final text.
func TestProtocols_NoHostToolsFallback(t *testing.T) {
	e := newEnv(t)
	s := e.start(hclient.DSH, "codex", hclient.Handler{}, nil)
	if f := prompt(t, s, frame("U0CLAUDE", fakeharness.TagSplit+" ...")); !strings.Contains(f, fakeharness.TagAgree) {
		t.Fatalf("fallback final %q", f)
	}
}

func TestHandoffDropMutation(t *testing.T) {
	e := newEnv(t)
	var got map[string]any
	s := e.start(hclient.DSH, "claude", hclient.Handler{
		Question: func(hclient.Question) (string, bool) { return "lines/words/bytes", true },
		HostTool: func(name string, a map[string]any) bool {
			if name == "plexus_delegate" {
				got = a
			}
			return true
		}}, func(o *hclient.Options) { o.HostTools = []string{"plexus_post", "plexus_ack", "plexus_delegate"} }, "HH_SIM_HANDOFF_DROP=evidence")
	prompt(t, s, frame("U0CODEX", fakeharness.TagAgree))
	if got == nil || got["evidence"] != nil || got["task"] == nil {
		t.Fatalf("drop mutation not applied: %v", got)
	}
}

// Stop behaviours: well-behaved long tasks honour the native interrupt and
// exit on stdin EOF; a misbehaving one ignores both and needs the tree kill.
func TestProtocols_InterruptAndExit(t *testing.T) {
	for _, p := range protos {
		for _, long := range []string{fakeharness.LongGen, fakeharness.LongTool, fakeharness.LongTick} {
			t.Run(p+"/"+long, func(t *testing.T) {
				e := newEnv(t)
				s := e.start(p, "dsh", hclient.Handler{}, nil, "HH_SIM_LONG=dsh="+long)
				done := make(chan error, 1)
				go func() { _, err := s.Prompt(frame("U0SIN", "<@U0DSH> [sim:long]"), ""); done <- err }()
				time.Sleep(400 * time.Millisecond)
				if err := s.Interrupt(); err != nil {
					t.Fatalf("interrupt: %v", err)
				}
				select {
				case err := <-done:
					if err != hclient.ErrInterrupted {
						t.Fatalf("turn end %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("well-behaved harness did not end the turn on interrupt")
				}
				s.CloseStdin()
				if !s.Exited(3 * time.Second) {
					t.Fatal("well-behaved harness did not exit within the 3 s grace after stdin closed")
				}
				if !e.has("-", "interrupt_received") || !e.has("-", "exit") {
					t.Fatalf("ledger lifecycle %v", e.phases("-"))
				}
			})
		}
	}
}

func TestMisbehavingHarnessNeedsKill(t *testing.T) {
	for _, p := range []string{hclient.Claude, hclient.DSH} {
		t.Run(p, func(t *testing.T) {
			e := newEnv(t)
			s := e.start(p, "claude", hclient.Handler{}, nil, "HH_SIM_MISBEHAVE=claude", "HH_SIM_LONG=claude=tick")
			go func() { _, _ = s.Prompt(frame("U0SIN", "<@U0CLAUDE> [sim:long]"), "") }()
			time.Sleep(400 * time.Millisecond)
			_ = s.Interrupt()
			s.CloseStdin()
			if s.Exited(1500 * time.Millisecond) {
				t.Fatal("misbehaving harness exited by itself")
			}
			if !e.has("-", "interrupt_ignored") {
				t.Fatalf("no interrupt_ignored: %v", e.phases("-"))
			}
			s.Kill()
			if !s.Exited(3 * time.Second) {
				t.Fatal("process group kill did not end it")
			}
		})
	}
}

func TestProtocols_Resume(t *testing.T) {
	for _, p := range protos {
		t.Run(p, func(t *testing.T) {
			e := newEnv(t)
			s := e.start(p, "codex", hclient.Handler{}, nil)
			prompt(t, s, frame("U0SIN", "<@U0CODEX> [sim:ping]"))
			id := s.ID()
			s.CloseStdin()
			s.Exited(3 * time.Second)
			s2 := e.start(p, "codex", hclient.Handler{}, func(o *hclient.Options) { o.ResumeID = id })
			if s2.ID() != id {
				t.Fatalf("resumed id %q want %q", s2.ID(), id)
			}
			if !e.has("-", "resumed") {
				t.Fatal("no resumed event")
			}
		})
	}
}

func TestCodexApprovalNeverAndWillRetry(t *testing.T) {
	e := newEnv(t)
	called := false
	s := e.start(hclient.Codex, "codex", hclient.Handler{Permission: func(hclient.Tool) bool { called = true; return false }},
		func(o *hclient.Options) { o.Approval = "never" })
	// The first turn also carries an error{willRetry:true} that must not end it.
	if f := prompt(t, s, frame("U0OTHER", "<@U0CODEX> [sim:member-write]")); !strings.Contains(f, fakeharness.TagDone) {
		t.Fatalf("final %q", f)
	}
	if called {
		t.Fatal("approvalPolicy=never must not ask")
	}
}

func TestPromptLogAndPlainFinal(t *testing.T) {
	e := newEnv(t)
	log := filepath.Join(e.dir, "prompts.jsonl")
	s := e.start(hclient.Claude, "dsh", hclient.Handler{}, nil, "HH_SIM_PROMPTLOG="+log)
	f := prompt(t, s, frame("U0SIN", "<@U0DSH> #目标 x"))
	if json.Valid([]byte(f)) {
		t.Fatalf("final must be prose, not JSON: %q", f)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "#目标") {
		t.Fatal("prompt log missing")
	}
}
