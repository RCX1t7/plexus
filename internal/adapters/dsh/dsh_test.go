package dsh

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

// Turn authority: Turn.Guest is the only flag (owner = false, stranger = true).
const (
	owner = false
	guest = true
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	// The plugin is bundled, so every StartSession installs the profile into
	// DSH_HOME. Point it at a scratch dir to keep the real ~/.dsh untouched.
	dir, _ := os.MkdirTemp("", "dsh-home-")
	os.Setenv("DSH_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestGateAndGuestOverTheFake drives the dangerous-action gate and the guest
// lock through the DSH fake (internal/fakes, mode dsh).
func TestGateAndGuestOverTheFake(t *testing.T) {
	o := fakes.Options(t, "dsh")
	o.HostTools = []harness.ToolSpec{{Name: "plexus_post"}}
	s, err := Adapter{}.StartSession(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A side-effecting call is forwarded for a decision; a parked deny carries a reason.
	parked := fakes.Driver{Decide: func(harness.PermissionRequest) harness.Decision {
		return harness.Decision{Allow: false, Reason: "已暂挂，等 Sin 批准"}
	}}
	if fin, _ := parked.Turn(t, s, "PERM git push --force origin main", owner); fin.Text != "gate:false:已暂挂，等 Sin 批准" {
		t.Fatalf("gate parked: %q", fin.Text)
	}
	// The classifier gets the typed command through the event.
	var gotCmd string
	seen := fakes.Driver{Decide: func(pr harness.PermissionRequest) harness.Decision {
		gotCmd = pr.Tool.Command
		return harness.Decision{Allow: true}
	}}
	if fin, _ := seen.Turn(t, s, "PERM git -C sub push -f", owner); fin.Text != "gate:true:" || gotCmd != "git -C sub push -f" {
		t.Fatalf("gate allow: %q cmd=%q", fin.Text, gotCmd)
	}
	// A stranger's (Turn.Guest) turn: every native tool -- shell, write and
	// read alike -- is denied natively by the bridge and never reaches the
	// Plexus gate; only the plexus_post host tool stays open.
	asked := false
	guard := fakes.Driver{
		Decide: func(harness.PermissionRequest) harness.Decision { asked = true; return harness.Decision{Allow: true} },
		Host:   func(ev harness.Event) harness.HostResult { return harness.HostResult{Text: ev.Name} },
	}
	for prompt, want := range map[string]string{
		"TOOL write /etc/passwd":     "guest-denied:write",
		"TOOL read notes.txt":        "guest-denied:read",
		"PERM rm -rf build":          "guest-denied:bash",
		`HOST plexus_task {"a":1}`:   "guest-denied:plexus_task",
		`HOST plexus_post {"t":"x"}`: "host:plexus_post:false",
	} {
		if fin, _ := guard.Turn(t, s, prompt, guest); fin.Text != want {
			t.Errorf("guest %q: got %q, want %q", prompt, fin.Text, want)
		}
	}
	if asked {
		t.Fatal("guest turn reached the Plexus permission gate; the native lock must deny first")
	}
}

// TestPromptWireCarriesGuestNotLevel: plexus.prompt sends Turn.Guest as
// "guest" and no "level" (the fake also rejects level with -32602).
func TestPromptWireCarriesGuestNotLevel(t *testing.T) {
	log := filepath.Join(t.TempDir(), "fake.log")
	o := fakes.Options(t, "dsh", "PLEXUS_FAKE_LOG="+log)
	s, err := Adapter{}.StartSession(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fakes.Driver{}.Turn(t, s, "as owner", owner)
	fakes.Driver{}.Turn(t, s, "as guest", guest)
	b, _ := os.ReadFile(log + ".msgs")
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(line), &m) == nil && m.Method == "plexus.prompt" {
			got = append(got, m.Params)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 prompts, got %d: %s", len(got), b)
	}
	for i, wantGuest := range []bool{owner, guest} {
		if _, has := got[i]["level"]; has {
			t.Errorf("prompt %d still carries level: %v", i, got[i])
		}
		if g, ok := got[i]["guest"].(bool); !ok || g != wantGuest {
			t.Errorf("prompt %d guest = %v, want %v", i, got[i]["guest"], wantGuest)
		}
	}
}

// TestEarlyFinalDoesNotCancelPrompt is review must-fix #13: the bridge streams
// the turn's final before answering plexus.prompt. The final ends the turn
// (cancelling the turn ctx); plexus.prompt must still complete on the caller's
// ctx, so Send succeeds and the final is delivered, not "context canceled".
func TestEarlyFinalDoesNotCancelPrompt(t *testing.T) {
	s, err := Adapter{}.StartSession(context.Background(), fakes.Options(t, "dsh"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		if fin, _ := (fakes.Driver{}).Turn(t, s, "FASTFINAL", owner); fin.Kind != harness.EventFinal || fin.Text != "fast" {
			t.Fatalf("run %d: %+v", i, fin)
		}
	}
	// the session is still usable afterwards
	if fin, _ := (fakes.Driver{}).Turn(t, s, "hi", owner); fin.Text != "echo: hi" {
		t.Fatalf("after: %q", fin.Text)
	}
}

// TestModelEffortPassthrough: SessionOptions.Model/Effort reach
// plexus.session.open; empty ones are omitted so the bridge resolves DSH's own
// configured default, and a turn still completes.
func TestModelEffortPassthrough(t *testing.T) {
	cases := []struct {
		name, model, effort, want string
	}{
		{"set", "deepseek-reasoner", "high", "model=deepseek-reasoner;effort=high;has_model=true;has_effort=true"},
		{"model only", "deepseek-chat", "", "model=deepseek-chat;effort=;has_model=true;has_effort=false"},
		{"empty = DSH default", "", "  ", "model=;effort=;has_model=false;has_effort=false"},
	}
	for _, c := range cases {
		o := fakes.Options(t, "dsh")
		o.Model, o.Effort = c.model, c.effort
		s, err := Adapter{}.StartSession(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if fin, _ := (fakes.Driver{}).Turn(t, s, "OPENED", owner); fin.Text != c.want {
			t.Errorf("%s: %q, want %q", c.name, fin.Text, c.want)
		}
		if fin, _ := (fakes.Driver{}).Turn(t, s, "hi", owner); fin.Text != "echo: hi" {
			t.Errorf("%s: turn did not complete: %q", c.name, fin.Text)
		}
		s.Close()
	}
}

// TestRefusesWhenGuestLockNotNative is the fail-closed start-up rule (CR-6 /
// SKELETON-REVIEW item 14): DSH accepts strangers, so the adapter must refuse
// the whole session unless the bridge reports guest_lock=native. The NOGUARD
// fake models a bridge that could not install the guest guard -- it answers
// plexus.initialize with {protocol:1} and no capabilities. A missing/empty
// guest_lock must be treated exactly like a non-native one: refuse.
func TestRefusesWhenGuestLockNotNative(t *testing.T) {
	o := fakes.Options(t, "dsh", "PLEXUS_FAKE_DSH_NOGUARD=1")
	s, err := Adapter{}.StartSession(context.Background(), o)
	if err == nil {
		s.Close()
		t.Fatal("expected StartSession to refuse when guest_lock is not native, got nil error")
	}
	if !strings.Contains(err.Error(), "guest lock") {
		t.Fatalf("refusal error should mention the guest lock, got: %v", err)
	}
	t.Logf("refusal: %v", err)
}

func TestBridgeTurnPermissionHostToolSteer(t *testing.T) {
	o := fakes.Options(t, "dsh")
	o.HostTools = []harness.ToolSpec{{Name: "plexus_post"}}
	s, err := Adapter{}.StartSession(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// events race ahead of the plexus.prompt reply: they must still map to the turn
	fin, evs := fakes.Driver{}.Turn(t, s, "hi", owner)
	if fin.Text != "echo: hi" || evs[0].Kind != harness.EventTextDelta || evs[0].TurnID == "" {
		t.Fatalf("%+v %+v", fin, evs)
	}
	d := fakes.Driver{Decide: func(p harness.PermissionRequest) harness.Decision { return harness.Decision{Allow: false} }}
	if fin, _ := d.Turn(t, s, "TOOL read a", owner); fin.Text != "allow:false" {
		t.Fatalf("%q", fin.Text)
	}
	h := fakes.Driver{Host: func(ev harness.Event) harness.HostResult { return harness.HostResult{Text: ev.Name} }}
	if fin, _ := h.Turn(t, s, `HOST plexus_post {"text":"x"}`, owner); fin.Text != "host:plexus_post:false" {
		t.Fatalf("%q", fin.Text)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = s.(harness.Steerer).Steer(context.Background(), "more")
	}()
	if fin, _ := (fakes.Driver{}).Turn(t, s, "SLOW", owner); fin.Text != "steered: more" {
		t.Fatalf("%q", fin.Text)
	}
}

func TestResumeOfLeasedSessionIsRefused(t *testing.T) {
	o := fakes.Options(t, "dsh")
	o.ResumeID = "busy"
	if _, err := (Adapter{}).StartSession(context.Background(), o); !errors.Is(err, harness.ErrActiveElsewhere) {
		t.Fatalf("%v", err)
	}
}

// TestOversizeFrameIsDroppedAndReported: a >32 MiB line from the bridge is
// discarded, reported once as a session-level "frame_dropped" error event,
// and the session keeps going (same turn reaches its final; next turn works).
func TestOversizeFrameIsDroppedAndReported(t *testing.T) {
	s, err := Adapter{}.StartSession(context.Background(), fakes.Options(t, "dsh"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fin, evs := fakes.Driver{}.Turn(t, s, "BIGFRAME", owner)
	if fin.Kind != harness.EventFinal || fin.Text != "after-bigframe" {
		t.Fatalf("turn after the oversize frame: %+v", fin)
	}
	drops := 0
	for _, ev := range evs {
		if ev.Kind == harness.EventError && ev.Status == "frame_dropped" {
			drops++
			if ev.TurnID != "" || !strings.Contains(ev.Text, "dropped one") {
				t.Errorf("dropped-frame event: %+v", ev)
			}
		}
	}
	if drops != 1 {
		t.Fatalf("want 1 frame_dropped event, got %d", drops)
	}
	if fin, _ := (fakes.Driver{}).Turn(t, s, "hi", owner); fin.Text != "echo: hi" {
		t.Fatalf("session did not continue: %q", fin.Text)
	}
}

// Effort is passed through plexus.session.open, so it is declared Native.
func TestCapabilitiesEffortNative(t *testing.T) {
	if got := (Adapter{}).Capabilities().Effort; got != harness.Native {
		t.Fatalf("Effort = %v, want Native", got)
	}
}
