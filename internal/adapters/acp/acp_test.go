package acp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/fakes"
	"github.com/RCX1t7/plexus/internal/harness"
)

func TestMain(m *testing.M) { fakes.MaybeRun(); os.Exit(m.Run()) }

func TestACPTurnPermissionCancel(t *testing.T) {
	a := New(Config{Name: "fake_acp"})
	if a.Capabilities().HostTools != harness.Unsupported {
		t.Fatal("ACP has no host tools")
	}
	s, err := a.StartSession(context.Background(), fakes.Options(t, "acp"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fin, _ := (fakes.Driver{}).Turn(t, s, "hi", false); fin.Text != "echo: hi" {
		t.Fatalf("%+v", fin)
	}
	var kind harness.ToolKind
	d := fakes.Driver{Decide: func(p harness.PermissionRequest) harness.Decision {
		kind = p.Tool.Kind
		return harness.Decision{Allow: false}
	}}
	if fin, _ := d.Turn(t, s, "TOOL shell ls", false); fin.Text != "outcome:no" || kind != harness.ToolShell {
		t.Fatalf("%q %s", fin.Text, kind)
	}
	go func() { time.Sleep(100 * time.Millisecond); _ = s.Interrupt(context.Background()) }()
	if fin, _ := (fakes.Driver{}).Turn(t, s, "SLOW", false); fin.Kind != harness.EventError && fin.Kind != harness.EventFinal {
		t.Fatalf("%+v", fin)
	}
}
