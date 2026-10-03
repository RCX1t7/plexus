package slackbot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/store"
)

// busyHarness reports every session as open in another app.
type busyHarness struct{ fakeHarness }

func (*busyHarness) ActiveElsewhere(_ harness.Env, id string, _ time.Time) (string, bool) {
	return "Claude Code process 4242 has it open", id != ""
}

func TestResumeRefusedWhenOpenElsewhere(t *testing.T) {
	tm := newTeam(t, false)
	a := tm.alpha
	h := &busyHarness{}
	w := &Worker{Bot: a.Bot, Harness: h, Store: a.Store, Outbox: a.Outbox, Policy: a.Policy, Owners: a.Owners,
		Peers: a.Peers, Stops: &Stops{}, SelfID: a.SelfID, OriginWait: a.OriginWait}
	_ = tm.st.SaveSession(a.Bot.Name, "C1:1.0", store.Session{NativeID: "sess-1"})
	w.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> continue"})
	eventually(t, "busy note", func() bool { return tm.fp.count("I did not resume this conversation") == 1 })
	if !strings.Contains(tm.fp.all()[0].Text, "4242") || len(h.turns()) != 0 {
		t.Fatalf("%+v %d", tm.fp.all(), len(h.turns()))
	}
	// a thread without a stored session starts fresh: nothing to collide with
	w.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.0", User: sin, Text: "<@" + alpha + "> hi"})
	eventually(t, "fresh turn", func() bool { return len(h.turns()) == 1 })
}

func TestWorkdirNoteIsWarningOnly(t *testing.T) {
	tm := newTeam(t, false)
	dir := t.TempDir()
	tm.alpha.LockDir = dir
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> hi"})
	eventually(t, "turn", func() bool { return tm.fp.count("done: hi") == 1 })
	locks, _ := filepath.Glob(filepath.Join(dir, "workdir-*.lock"))
	if len(locks) != 1 {
		t.Fatal(locks)
	}
	var n workdirNote
	b, _ := os.ReadFile(locks[0])
	if json.Unmarshal(b, &n) != nil || n.PID != os.Getpid() || n.Thread != "C1:1.0" {
		t.Fatalf("%s", b)
	}
	// a second thread in the same workdir still runs (no exclusive lock)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.0", User: sin, Text: "<@" + alpha + "> again"})
	eventually(t, "second turn", func() bool { return tm.fp.count("done: again") == 1 })
}
