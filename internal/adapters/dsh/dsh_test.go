package dsh

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/fakes"
	"github.com/RCX1t7/plexus/internal/harness"
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
	if fin, _ := parked.Turn(t, s, "PERM git push --force origin main", harness.LevelFull); fin.Text != "gate:false:已暂挂，等 Sin 批准" {
		t.Fatalf("gate parked: %q", fin.Text)
	}
	// The classifier gets the typed command through the event.
	var gotCmd string
	seen := fakes.Driver{Decide: func(pr harness.PermissionRequest) harness.Decision {
		gotCmd = pr.Tool.Command
		return harness.Decision{Allow: true}
	}}
	if fin, _ := seen.Turn(t, s, "PERM git -C sub push -f", harness.LevelFull); fin.Text != "gate:true:" || gotCmd != "git -C sub push -f" {
		t.Fatalf("gate allow: %q cmd=%q", fin.Text, gotCmd)
	}
	// A stranger's (guest) turn: a non-read native tool is denied natively, never asked.
	asked := false
	guard := fakes.Driver{Decide: func(harness.PermissionRequest) harness.Decision { asked = true; return harness.Decision{Allow: true} }}
	if fin, _ := guard.Turn(t, s, "TOOL write /etc/passwd", harness.LevelChat); fin.Text != "guest-denied:write" || asked {
		t.Fatalf("guest lock: %q asked=%v", fin.Text, asked)
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
	fin, evs := fakes.Driver{}.Turn(t, s, "hi", harness.LevelFull)
	if fin.Text != "echo: hi" || evs[0].Kind != harness.EventTextDelta || evs[0].TurnID == "" {
		t.Fatalf("%+v %+v", fin, evs)
	}
	d := fakes.Driver{Decide: func(p harness.PermissionRequest) harness.Decision { return harness.Decision{Allow: false} }}
	if fin, _ := d.Turn(t, s, "TOOL read a", harness.LevelFull); fin.Text != "allow:false" {
		t.Fatalf("%q", fin.Text)
	}
	h := fakes.Driver{Host: func(ev harness.Event) harness.HostResult { return harness.HostResult{Text: ev.Name} }}
	if fin, _ := h.Turn(t, s, `HOST plexus_post {"text":"x"}`, harness.LevelFull); fin.Text != "host:plexus_post:false" {
		t.Fatalf("%q", fin.Text)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = s.(harness.Steerer).Steer(context.Background(), "more")
	}()
	if fin, _ := (fakes.Driver{}).Turn(t, s, "SLOW", harness.LevelFull); fin.Text != "steered: more" {
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
