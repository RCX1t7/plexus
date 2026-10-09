package slackbot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/policy"
	"github.com/RCX1t7/plexus/internal/store"
)

const (
	sin   = "U0SIN0001"
	alpha = "U0ALPHA01"
	beta  = "U0BETA001"
	other = "U0STRANGE"
)

type team struct {
	st      *store.Store
	fp      *fakePoster
	peers   *Peers
	stops   *Stops
	alpha   *Worker
	beta    *Worker
	ha, hb  *fakeHarness
	ctx     context.Context
	restart func()
}

func newTeam(t testing.TB, hostTools bool) *team {
	t.Helper()
	StopGrace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tm := &team{st: openStore(t), fp: &fakePoster{}, peers: &Peers{}, stops: &Stops{}, ctx: ctx,
		ha: &fakeHarness{hostTools: hostTools}, hb: &fakeHarness{hostTools: hostTools}}
	tm.peers.Add(alpha, "alpha")
	tm.peers.Add(beta, "beta")
	mk := func(name, id string, h *fakeHarness) *Worker {
		wd := t.TempDir()
		return &Worker{Bot: config.Bot{Name: name, Workdir: wd}, Harness: h, Store: tm.st,
			Outbox: &Outbox{Bot: name, Store: tm.st, Poster: tm.fp},
			Policy: policy.Policy{GOOS: "linux", Workdir: wd, StrangerGuard: true, Revoker: tm.st},
			Owners: []string{sin}, Peers: tm.peers, Stops: tm.stops, SelfID: id, OriginWait: 100 * time.Millisecond}
	}
	tm.alpha, tm.beta = mk("alpha", alpha, tm.ha), mk("beta", beta, tm.hb)
	tm.stops.Register(tm.alpha)
	tm.stops.Register(tm.beta)
	// restart simulates a Plexus restart: fresh workers, harnesses and
	// in-memory state over the same database.
	tm.restart = func() {
		tm.ha, tm.hb = &fakeHarness{hostTools: hostTools}, &fakeHarness{hostTools: hostTools}
		tm.stops = &Stops{}
		tm.alpha, tm.beta = mk("alpha", alpha, tm.ha), mk("beta", beta, tm.hb)
		tm.alpha.Stops, tm.beta.Stops = tm.stops, tm.stops
		tm.stops.Register(tm.alpha)
		tm.stops.Register(tm.beta)
	}
	return tm
}

func (tm *team) both(in Inbound) {
	tm.alpha.Handle(tm.ctx, in)
	tm.beta.Handle(tm.ctx, in)
}

func TestSinReplyAndReplayDedup(t *testing.T) {
	tm := newTeam(t, false)
	in := Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> hello"}
	tm.alpha.Handle(tm.ctx, in)
	eventually(t, "reply", func() bool { return tm.fp.count("done: hello") == 1 })
	tm.alpha.Handle(tm.ctx, in) // Slack retry / reconnect replay
	time.Sleep(100 * time.Millisecond)
	if n := len(tm.ha.turns()); n != 1 {
		t.Fatalf("replay started %d turns", n)
	}
	turn := tm.ha.turns()[0]
	if turn.Guest || !strings.Contains(turn.Text, "Sin (owner)") {
		t.Fatalf("%+v", turn)
	}
	if p := tm.fp.all()[0]; p.Thread != "1.0" {
		t.Fatalf("reply not threaded: %+v", p)
	}
}

func TestStrangerIsGuarded(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: other, Text: "<@" + alpha + "> TOOL shell"})
	eventually(t, "reply", func() bool { return tm.fp.count("allow=false") == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: other, Text: "<@" + alpha + "> TOOL read"})
	eventually(t, "read denied too", func() bool { return tm.fp.count("allow=false") == 2 })
	if !tm.ha.turns()[0].Guest {
		t.Fatal("stranger turn not chat-only")
	}
	// "stop" from a stranger is just a message
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: other, Text: "<@" + alpha + "> stop"})
	eventually(t, "stop echo", func() bool { return tm.fp.count("done: stop") == 1 })
	if tm.fp.count(StopAck) != 0 {
		t.Fatal("stranger stopped a task")
	}
}

func TestPartnerInheritsTrustOfWritingTurn(t *testing.T) {
	tm := newTeam(t, false)
	// beta's post written in a stranger's turn
	_ = tm.st.PutOrigin("C1", "2.0", store.Origin{Bot: "beta", Source: string(policy.FromStranger)})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.0", User: beta, Text: "<@" + alpha + "> TOOL shell"})
	eventually(t, "laundered request denied", func() bool { return tm.fp.count("allow=false") == 1 })
	// beta's post written in Sin's turn
	_ = tm.st.PutOrigin("C1", "3.0", store.Origin{Bot: "beta", Source: string(policy.FromSin)})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "3.0", User: beta, Text: "<@" + alpha + "> TOOL shell ls"})
	eventually(t, "trusted partner allowed", func() bool { return tm.fp.count("allow=true") == 1 })
	// no origin record (e.g. posted by hand or lost): fail closed
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "4.0", User: beta, Text: "<@" + alpha + "> TOOL write"})
	eventually(t, "unknown origin denied", func() bool { return tm.fp.count("allow=false") == 2 })
	if !strings.Contains(tm.ha.turns()[1].Text, "a partner (beta)") {
		t.Fatal(tm.ha.turns()[1].Text)
	}
}

func TestStopTreeAcrossPartnersOneAck(t *testing.T) {
	tm := newTeam(t, false)
	tm.both(Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> <@" + beta + "> SLOW"})
	eventually(t, "both running", func() bool { return len(tm.ha.turns()) == 1 && len(tm.hb.turns()) == 1 })
	tm.both(Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: sin, Text: "停"})
	eventually(t, "ack", func() bool { return tm.fp.count(StopAck) == 1 })
	eventually(t, "sessions closed", func() bool {
		return tm.ha.sessions[0].isClosed() && tm.hb.sessions[0].isClosed()
	})
	if tm.ha.sessions[0].interrupts == 0 || tm.hb.sessions[0].interrupts == 0 {
		t.Fatal("native interrupt not sent")
	}
	time.Sleep(150 * time.Millisecond)
	if n := tm.fp.count(StopAck); n != 1 {
		t.Fatalf("%d acks", n)
	}
	if tm.fp.count("interrupted") != 0 || tm.fp.count("⚠️") != 0 {
		t.Fatalf("stopped turn still posted: %+v", tm.fp.all())
	}
	if r, _ := tm.st.AnyRevoked("C1:1.0"); !r {
		t.Fatal("root not revoked")
	}
	// a partner cannot restart the stopped tree
	_ = tm.st.PutOrigin("C1", "1.6", store.Origin{Bot: "beta", Source: string(policy.FromSin)})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.6", ThreadTS: "1.0", User: beta, Text: "<@" + alpha + "> go on"})
	time.Sleep(150 * time.Millisecond)
	if len(tm.ha.turns()) != 1 {
		t.Fatal("stopped tree started a turn")
	}
	// Sin speaks again: a new root, same native session resumed
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.7", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> again"})
	eventually(t, "new turn", func() bool { return tm.fp.count("done: again") == 1 })
	if s := tm.ha.sessions[len(tm.ha.sessions)-1]; s.opts.ResumeID != "native-1" {
		t.Fatalf("not resumed: %q", s.opts.ResumeID)
	}
	if root := tm.alpha.rootOf("C1:1.0"); root != "C1:1.7" {
		t.Fatalf("root = %s", root)
	}
}

func TestPlexusStopCommand(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> SLOW"})
	eventually(t, "running", func() bool { return len(tm.ha.turns()) == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C2", TS: "8.0", User: sin, Text: "<@" + alpha + "> plexus stop C1:1.0"})
	eventually(t, "ack", func() bool { return tm.fp.count(StopAck) == 1 })
	eventually(t, "closed", func() bool { return tm.ha.sessions[0].isClosed() })
}

func TestSteerOrQueueNeverBusy(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> SLOW"})
	eventually(t, "running", func() bool { return len(tm.ha.turns()) == 1 })
	// a stranger's message is queued, never merged into Sin's turn
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: other, Text: "<@" + alpha + "> hi"})
	// Sin's follow-up is steered into the running turn
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> also X"})
	eventually(t, "steered reply", func() bool { return tm.fp.count("steered: also X") == 1 })
	eventually(t, "queued stranger turn", func() bool { return tm.fp.count("done: hi") == 1 })
	if n := len(tm.ha.turns()); n != 2 {
		t.Fatalf("%d turns", n)
	}
	for _, p := range tm.fp.all() {
		if strings.Contains(strings.ToLower(p.Text), "busy") {
			t.Fatal("busy reply")
		}
	}
}

func TestHostToolDelegateWritesHandoff(t *testing.T) {
	tm := newTeam(t, true)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin,
		Text: `<@` + alpha + `> HOST plexus_delegate {"to":"beta","task":"write tests","done_when":"go test passes","inputs":["a.go"]}`})
	eventually(t, "card", func() bool { return tm.fp.count("📋 *Handoff*") == 1 })
	var card posted
	for _, p := range tm.fp.all() {
		if strings.Contains(p.Text, "Handoff") {
			card = p
		}
	}
	if !strings.HasPrefix(card.Text, "<@"+beta+">") || !strings.Contains(card.Text, "If stuck:* <@"+alpha+">") {
		t.Fatalf("%s", card.Text)
	}
	o := originOf(t, tm, "C1", card.TS)
	if o.Source != string(policy.FromSin) || o.Handoff == "" {
		t.Fatalf("origin %+v", o)
	}
	h, ok, _ := tm.st.GetHandoff(o.Handoff)
	if !ok || h.ToBot != "beta" || h.ParentTask != "C1:1.0" || !strings.Contains(string(h.Record), `"done_when":"go test passes"`) {
		t.Fatalf("%+v", h)
	}
	// beta receives the card with Sin's trust and the record in its prompt
	tm.beta.Handle(tm.ctx, Inbound{Channel: "C1", TS: card.TS, ThreadTS: "1.0", User: alpha, Text: card.Text})
	eventually(t, "beta turn", func() bool { return len(tm.hb.turns()) == 1 })
	bt := tm.hb.turns()[0]
	if bt.Guest || !strings.Contains(bt.Text, "[Handoff record") {
		t.Fatalf("%+v", bt)
	}
	// missing done_when is refused
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.3", ThreadTS: "1.0", User: sin,
		Text: `<@` + alpha + `> HOST plexus_delegate {"to":"beta","task":"x"}`})
	eventually(t, "refused", func() bool { return tm.fp.count("host: missing done_when") == 1 })
}

func TestStrangerTurnCannotDelegateOrDeliver(t *testing.T) {
	tm := newTeam(t, true)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: other,
		Text: `<@` + alpha + `> HOST plexus_delegate {"to":"beta","task":"x","done_when":"y"}`})
	eventually(t, "refused", func() bool { return tm.fp.count("outside the team") == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.0", User: other, Text: `<@` + alpha + `> HOST plexus_post {"text":"hello"}`})
	eventually(t, "post allowed", func() bool { return tm.fp.count("hello") == 1 })
}

func TestStopTreeHostToolOnlyForSin(t *testing.T) {
	tm := newTeam(t, true)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: other, Text: "<@" + alpha + "> HOST plexus_stop_tree {}"})
	eventually(t, "refused", func() bool { return tm.fp.count("outside the team") == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.0", User: sin, Text: "<@" + alpha + "> HOST plexus_stop_tree {}"})
	eventually(t, "ack", func() bool { return tm.fp.count(StopAck) == 1 })
}

func TestAutonomousTurnsAndLoopGuard(t *testing.T) {
	tm := newTeam(t, true)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> QUIET"})
	eventually(t, "first", func() bool { return len(tm.ha.turns()) == 1 })
	time.Sleep(50 * time.Millisecond)
	// a thread message not addressed to alpha: an autonomous turn whose final text is not posted
	_ = tm.st.PutOrigin("C1", "1.1", store.Origin{Bot: "beta", Source: "sin"})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: beta, Text: "ping"})
	eventually(t, "autonomous turn", func() bool { return len(tm.ha.turns()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if tm.fp.count("done: ping") != 0 {
		t.Fatal("autonomous final text was posted")
	}
	if !strings.Contains(tm.ha.turns()[1].Text, "not addressed to you") {
		t.Fatal(tm.ha.turns()[1].Text)
	}
	// identical chatter again with no work in between is not delivered
	_ = tm.st.PutOrigin("C1", "1.2", store.Origin{Bot: "beta", Source: "sin"})
	_ = tm.st.PutOrigin("C1", "1.3", store.Origin{Bot: "beta", Source: "sin"})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: beta, Text: "PING "})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.3", ThreadTS: "1.0", User: beta, Text: "ping"})
	time.Sleep(200 * time.Millisecond)
	if n := len(tm.ha.turns()); n != 2 {
		t.Fatalf("loop guard let %d turns through", n)
	}
	// after real work the same words are delivered again
	_ = tm.st.PutOrigin("C1", "1.4", store.Origin{Bot: "beta", Source: "sin"})
	_ = tm.st.PutOrigin("C1", "1.5", store.Origin{Bot: "beta", Source: "sin"})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.4", ThreadTS: "1.0", User: beta, Text: "WORK"})
	eventually(t, "work turn", func() bool { return len(tm.ha.turns()) == 3 })
	time.Sleep(50 * time.Millisecond)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: beta, Text: "ping"})
	eventually(t, "ping after work", func() bool { return len(tm.ha.turns()) == 4 })
}

func TestRecoverInflight(t *testing.T) {
	tm := newTeam(t, false)
	_ = tm.st.SaveSession("alpha", "C1:1.0", store.Session{NativeID: "native-1", RootTask: "C1:1.0", Channel: "C1", ThreadTS: "1.0"})
	_ = tm.st.UpdateSession("alpha", "C1:1.0", func(s *store.Session) { s.Inflight, s.InflightSource, s.InflightUser = "1.0", "sin", sin })
	_ = tm.st.PutQuestion(store.Question{Thread: "C1:1.0", Bot: "alpha", QID: "q", State: store.QuestionOpen})
	tm.alpha.Recover(tm.ctx)
	eventually(t, "recovery turn", func() bool { return len(tm.ha.turns()) == 1 })
	if !strings.Contains(tm.ha.turns()[0].Text, "Plexus restarted") || tm.ha.sessions[0].opts.ResumeID != "native-1" {
		t.Fatalf("%+v", tm.ha.turns()[0])
	}
	if q, _ := tm.st.OpenQuestions("alpha"); len(q) != 0 {
		t.Fatal("stale question not retired")
	}
}

func TestNoGuestLockRefusesStrangers(t *testing.T) {
	tm := newTeam(t, false)
	tm.ha.noGuestLock = true // e.g. Codex: read-only sandbox still runs "safe" commands unasked
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: other, Text: "<@" + alpha + "> cat .env"})
	eventually(t, "notice", func() bool { return tm.fp.count("only take requests from my team") == 1 })
	if len(tm.ha.turns()) != 0 {
		t.Fatal("stranger turn started on a harness without GuestLock")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> hi"})
	eventually(t, "sin served", func() bool { return tm.fp.count("done: hi") == 1 })
}

// Review item 9: a partner @-mentioning this partner with the same chatter
// again and again, with no work in between, starts one turn, not six.
func TestMentionPingPongGuard(t *testing.T) {
	tm := newTeam(t, true)
	for i := 0; i < 6; i++ {
		ts := fmt.Sprintf("2.%d", i)
		_ = tm.st.PutOrigin("C1", ts, store.Origin{Bot: "beta", Source: "sin"})
		in := Inbound{Channel: "C1", TS: ts, User: beta, Text: "<@" + alpha + "> thanks!"}
		if i > 0 {
			in.ThreadTS = "2.0"
		}
		tm.alpha.Handle(tm.ctx, in)
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(tm.ha.turns()); n != 1 {
		t.Fatalf("ping-pong started %d turns", n)
	}
	// Sin is never filtered, and resets the guard
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.7", ThreadTS: "2.0", User: sin, Text: "<@" + alpha + "> thanks!"})
	eventually(t, "sin turn", func() bool { return len(tm.ha.turns()) == 2 })
	_ = tm.st.PutOrigin("C1", "2.8", store.Origin{Bot: "beta", Source: "sin"})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "2.8", ThreadTS: "2.0", User: beta, Text: "<@" + alpha + "> thanks!"})
	eventually(t, "partner after sin", func() bool { return len(tm.ha.turns()) == 3 })
}
