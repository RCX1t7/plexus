package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/fakes"
	"github.com/RCX1t7/plexus/internal/harness"
)

func TestMain(m *testing.M) { fakes.MaybeRun(); os.Exit(m.Run()) }

func start(t *testing.T, o harness.SessionOptions) harness.Session {
	t.Helper()
	s, err := Adapter{}.StartSession(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestArgsEnvAndInitialize(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SIMPLE", "1")
	log := filepath.Join(t.TempDir(), "log")
	o := fakes.Options(t, "claude", "PLEXUS_FAKE_LOG="+log)
	o.ResumeID, o.Persona = "-dash-id", "be nice"
	o.HostTools = []harness.ToolSpec{{Name: "plexus_post", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	s := start(t, o)
	fin, _ := fakes.Driver{}.Turn(t, s, "[Plexus · from <@U1>]\nhello", false)
	if fin.Kind != harness.EventFinal || fin.Text != "echo: hello" {
		t.Fatalf("%+v", fin)
	}
	var rec struct {
		Args               []string
		CLAUDE_CODE_SIMPLE string
	}
	b, _ := os.ReadFile(log)
	_ = json.Unmarshal(b, &rec)
	args := strings.Join(rec.Args, " ")
	for _, want := range []string{"--input-format stream-json", "--permission-prompt-tool stdio", "--resume=-dash-id",
		"--append-system-prompt be nice", `--mcp-config {"mcpServers":{"plexus":{"type":"sdk","name":"plexus"}}}`} {
		if !strings.Contains(args, want) {
			t.Fatalf("args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, " -p") || rec.CLAUDE_CODE_SIMPLE != "" {
		t.Fatalf("bare-mode risk: args=%s simple=%q", args, rec.CLAUDE_CODE_SIMPLE)
	}
	msgs, _ := os.ReadFile(log + ".msgs")
	if !strings.Contains(string(msgs), `"perTaskStopAffordance":true`) || !strings.Contains(string(msgs), `"sdkMcpServers":["plexus"]`) {
		t.Fatalf("initialize: %s", msgs)
	}
}

func TestPermissionHookAndCanUseTool(t *testing.T) {
	s := start(t, fakes.Options(t, "claude"))
	var kinds []harness.ToolKind
	d := fakes.Driver{Decide: func(p harness.PermissionRequest) harness.Decision {
		kinds = append(kinds, p.Tool.Kind)
		return harness.Decision{Allow: p.Tool.Kind == harness.ToolRead, Reason: "test"}
	}}
	fin, _ := d.Turn(t, s, "TOOL shell rm -rf", false)
	if fin.Text != "hook:deny can_use_tool:deny" {
		t.Fatalf("%q", fin.Text)
	}
	fin, _ = d.Turn(t, s, "TOOL read a.txt", false)
	if fin.Text != "hook:allow can_use_tool:allow" {
		t.Fatalf("%q", fin.Text)
	}
	if kinds[0] != harness.ToolShell || kinds[len(kinds)-1] != harness.ToolRead {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestQuestionRoundTrip(t *testing.T) {
	s := start(t, fakes.Options(t, "claude"))
	d := fakes.Driver{Answer: func(qs []harness.Question) harness.Answers {
		if len(qs) != 1 || len(qs[0].Options) != 2 {
			t.Fatalf("questions %+v", qs)
		}
		return harness.Answers{qs[0].ID: {"Postgres"}}
	}}
	if fin, _ := d.Turn(t, s, "ASK", false); fin.Text != "answer:allow:Postgres" {
		t.Fatalf("%q", fin.Text)
	}
}

func TestHostToolViaSdkMcp(t *testing.T) {
	o := fakes.Options(t, "claude")
	o.HostTools = []harness.ToolSpec{{Name: "plexus_post"}, {Name: "plexus_deliver"}}
	s := start(t, o)
	d := fakes.Driver{Host: func(ev harness.Event) harness.HostResult {
		in, _ := ev.Tool.Input.(map[string]any)
		return harness.HostResult{Text: ev.Name + "=" + in["text"].(string)}
	}}
	fin, _ := d.Turn(t, s, `HOST plexus_post {"text":"hi"}`, false)
	if fin.Text != "host:2:plexus_post=hi:false" {
		t.Fatalf("%q", fin.Text)
	}
}

func TestSteerAndInterrupt(t *testing.T) {
	s := start(t, fakes.Options(t, "claude"))
	st := s.(harness.Steerer)
	steered := false
	d := fakes.Driver{OnEvent: func(ev harness.Event) {
		if ev.Kind == harness.EventExtension && ev.Name == "system/init" && !steered {
			steered = true
			go func() { time.Sleep(50 * time.Millisecond); _ = st.Steer(context.Background(), "also do X") }()
		}
	}}
	if fin, _ := d.Turn(t, s, "SLOW", false); fin.Text != "steered: also do X" {
		t.Fatalf("%q", fin.Text)
	}
	d = fakes.Driver{OnEvent: func(ev harness.Event) {
		if ev.Kind == harness.EventExtension && ev.Name == "system/init" {
			go func() { time.Sleep(50 * time.Millisecond); _ = s.Interrupt(context.Background()) }()
		}
	}}
	if fin, _ := d.Turn(t, s, "SLOW", false); fin.Kind != harness.EventError {
		t.Fatalf("interrupt: %+v", fin)
	}
}

func TestBackgroundTasksAndStopTask(t *testing.T) {
	s := start(t, fakes.Options(t, "claude"))
	ts := s.(harness.TaskStopper)
	fakes.Driver{}.Turn(t, s, "BG", false)
	if got := ts.BackgroundTasks(); len(got) != 1 || got[0] != "task-1" {
		t.Fatalf("tasks after turn: %v", got)
	}
	if err := ts.StopTask(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	fakes.Driver{}.Turn(t, s, "BGDONE", false)
	deadline := time.Now().Add(2 * time.Second)
	for len(ts.BackgroundTasks()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ts.BackgroundTasks(); len(got) != 0 {
		t.Fatalf("finished task still listed: %v", got)
	}
}

func TestBareWarning(t *testing.T) {
	s := start(t, fakes.Options(t, "claude", "PLEXUS_FAKE_BARE=1"))
	_, evs := fakes.Driver{}.Turn(t, s, "hi", false)
	for _, e := range evs {
		if e.Name == "plexus.warning" {
			return
		}
	}
	t.Fatal("no bare-mode warning")
}

func TestPlexusHostToolsAreMeta(t *testing.T) {
	for _, n := range []string{"plexus_post", "plexus_delegate", "plexus_ack", "plexus_deliver", "plexus_review", "plexus_stop_tree"} {
		if k := toolRequest("mcp__plexus__"+n, map[string]any{}).Kind; k != harness.ToolMeta {
			t.Errorf("%s: kind %s", n, k)
		}
	}
	if k := toolRequest("mcp__other__plexus_post", map[string]any{}).Kind; k == harness.ToolMeta {
		t.Error("a foreign MCP server's tool became Meta")
	}
}
