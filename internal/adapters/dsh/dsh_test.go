package dsh

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/fakes"
	"github.com/RCX1t7/plexus/internal/harness"
)

func TestMain(m *testing.M) { fakes.MaybeRun(); os.Exit(m.Run()) }

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
