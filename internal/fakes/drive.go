package fakes

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

// Options returns session options that run the named fake.
func Options(t *testing.T, mode string, extra ...string) harness.SessionOptions {
	return harness.SessionOptions{Workdir: t.TempDir(), Exe: os.Args[0],
		Env: append([]string{"PLEXUS_FAKE=" + mode}, extra...)}
}

// Driver answers harness callbacks during a test turn.
type Driver struct {
	Decide func(harness.PermissionRequest) harness.Decision
	Answer func([]harness.Question) harness.Answers
	Host   func(harness.Event) harness.HostResult
	// OnEvent sees every event of the turn (e.g. to steer mid-turn).
	OnEvent func(harness.Event)
}

// Turn sends text and drives the session until the turn's final or error
// event; it returns that event and every event seen.
func (d Driver) Turn(t *testing.T, s harness.Session, text string, guest bool) (harness.Event, []harness.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := s.Send(ctx, harness.Turn{Text: text, Guest: guest})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var seen []harness.Event
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed; seen %d", len(seen))
			}
			seen = append(seen, ev)
			if d.OnEvent != nil {
				d.OnEvent(ev)
			}
			switch ev.Kind {
			case harness.EventPermission:
				dec := harness.Decision{Allow: true}
				if d.Decide != nil {
					dec = d.Decide(*ev.Perm)
				}
				ev.Decide(dec)
			case harness.EventQuestion:
				var a harness.Answers
				if d.Answer != nil {
					a = d.Answer(ev.Questions)
				}
				ev.Answer(a)
			case harness.EventHostTool:
				r := harness.HostResult{Text: "ok"}
				if d.Host != nil {
					r = d.Host(ev)
				}
				ev.Call(r)
			case harness.EventFinal, harness.EventError:
				if ev.TurnID == id {
					return ev, seen
				}
			}
		case <-ctx.Done():
			t.Fatalf("turn timed out; seen %d events", len(seen))
		}
	}
}
