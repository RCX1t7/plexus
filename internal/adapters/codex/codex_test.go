package codex

import (
	"context"
	"encoding/json"
	"errors"
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

func TestFinalAnswerPhaseAndWillRetry(t *testing.T) {
	s := start(t, fakes.Options(t, "codex"))
	fin, _ := fakes.Driver{}.Turn(t, s, "hello", harness.LevelFull)
	if fin.Kind != harness.EventFinal || fin.Text != "echo: hello" {
		t.Fatalf("%+v", fin) // the commentary message "thinking" must not win
	}
	fin, _ = fakes.Driver{}.Turn(t, s, "RETRY", harness.LevelFull)
	if fin.Kind != harness.EventFinal {
		t.Fatalf("willRetry error ended the turn: %+v", fin)
	}
}

func TestApprovalsAndQuestions(t *testing.T) {
	s := start(t, fakes.Options(t, "codex"))
	deny := fakes.Driver{Decide: func(p harness.PermissionRequest) harness.Decision {
		return harness.Decision{Allow: p.Tool.Kind == harness.ToolWrite && len(p.Tool.Paths) == 1 && p.Tool.Paths[0] == "a.go"}
	}}
	if fin, _ := deny.Turn(t, s, "TOOL shell rm", harness.LevelReadOnly); fin.Text != "decision:decline" {
		t.Fatalf("%q", fin.Text)
	}
	if fin, _ := deny.Turn(t, s, "TOOL write a.go", harness.LevelFull); fin.Text != "decision:accept" {
		t.Fatalf("%q", fin.Text)
	}
	ask := fakes.Driver{Answer: func(qs []harness.Question) harness.Answers { return harness.Answers{"q1": {"SQLite"}} }}
	if fin, _ := ask.Turn(t, s, "ASK", harness.LevelFull); !strings.Contains(fin.Text, `"q1":{"answers":["SQLite"]}`) {
		t.Fatalf("%q", fin.Text)
	}
}

func TestDynamicToolsSteerAndReadOnlySandbox(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	o := fakes.Options(t, "codex", "PLEXUS_FAKE_LOG="+log)
	o.HostTools = []harness.ToolSpec{{Name: "plexus_post"}}
	s := start(t, o)
	host := fakes.Driver{Host: func(ev harness.Event) harness.HostResult { return harness.HostResult{Text: "posted:" + ev.Name} }}
	if fin, _ := host.Turn(t, s, `HOST plexus_post {"text":"x"}`, harness.LevelReadOnly); fin.Text != "host:posted:plexus_post:true" {
		t.Fatalf("%q", fin.Text)
	}
	st := s.(harness.Steerer)
	steer := fakes.Driver{OnEvent: func(harness.Event) {}}
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(20 * time.Millisecond)
			if st.Steer(context.Background(), "[Plexus]\nmore") == nil {
				return
			}
		}
	}()
	if fin, _ := steer.Turn(t, s, "SLOW", harness.LevelFull); fin.Text != "steered: more" {
		t.Fatalf("%q", fin.Text)
	}
	msgs, _ := os.ReadFile(log + ".msgs")
	for _, want := range []string{`"experimentalApi":true`, `"dynamicTools":[{`, `"type":"readOnly"`, `"expectedTurnId":"tu-1"`} {
		if !strings.Contains(string(msgs), want) {
			t.Fatalf("missing %s in %s", want, msgs)
		}
	}
}

func TestInterrupt(t *testing.T) {
	s := start(t, fakes.Options(t, "codex"))
	d := fakes.Driver{}
	go func() { time.Sleep(100 * time.Millisecond); _ = s.Interrupt(context.Background()) }()
	if fin, _ := d.Turn(t, s, "SLOW", harness.LevelFull); fin.Kind != harness.EventError && fin.Kind != harness.EventFinal {
		t.Fatalf("%+v", fin)
	}
}

func TestResumeOfThreadWithActiveWriterIsRefused(t *testing.T) {
	o := fakes.Options(t, "codex")
	o.ResumeID = "busy"
	_, err := Adapter{}.StartSession(context.Background(), o)
	if !errors.Is(err, harness.ErrActiveElsewhere) {
		t.Fatalf("%v", err)
	}
	if (Adapter{}).IdleUnload() <= 0 {
		t.Fatal("codex threads must be unloaded when idle")
	}
}

func TestFileChangeDeletes(t *testing.T) {
	var it item
	_ = json.Unmarshal([]byte(`{"type":"fileChange","id":"i1","changes":[
		{"path":"a.go","kind":{"type":"update"}},
		{"path":"../x.go","kind":{"type":"delete"}},
		{"path":"/o/b.go","kind":{"type":"update","move_path":"b.go"}},
		{"path":"c.go","kind":"delete"}]}`), &it)
	r := itemRequest(it)
	if strings.Join(r.Deletes, ",") != "../x.go,/o/b.go,c.go" || len(r.Paths) != 5 {
		t.Fatalf("%+v", r)
	}
	r = legacyPatch(map[string]any{"fileChanges": map[string]any{
		"../gone.txt": map[string]any{"delete": map[string]any{"content": "x"}},
		"keep.txt":    map[string]any{"update": map[string]any{"unified_diff": "", "move_path": nil}},
		"/o/m.txt":    map[string]any{"update": map[string]any{"move_path": "m.txt"}},
	}})
	if strings.Join(r.Deletes, ",") != "../gone.txt,/o/m.txt" || r.Kind != harness.ToolWrite {
		t.Fatalf("%+v", r)
	}
}

func TestParkedDeclineSteersReason(t *testing.T) {
	s := start(t, fakes.Options(t, "codex"))
	park := fakes.Driver{Decide: func(harness.PermissionRequest) harness.Decision {
		return harness.Decision{Allow: false, Reason: "已暂挂，等 Sin 批准 (parked)"}
	}}
	if fin, _ := park.Turn(t, s, "TOOL shell git-push-f", harness.LevelFull); fin.Text != "decision:decline" {
		t.Fatalf("%q", fin.Text)
	}
	// The fake hands the next SLOW turn whatever was steered: the reason.
	fin, _ := fakes.Driver{}.Turn(t, s, "SLOW", harness.LevelFull)
	if !strings.Contains(fin.Text, "已暂挂，等 Sin 批准") || !strings.Contains(fin.Text, "git-push-f") {
		t.Fatalf("%q", fin.Text)
	}
}
