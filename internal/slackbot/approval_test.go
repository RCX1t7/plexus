package slackbot

import (
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/store"
)

func card(tm *team) (posted, bool) {
	for _, p := range tm.fp.all() {
		if p.Meta.Kind == "approval" {
			return p, true
		}
	}
	return posted{}, false
}

func TestDangerousActionWaitsForSinOnly(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push --force origin main"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	c, _ := card(tm)
	if len(c.Meta.Blocks) == 0 || !strings.Contains(c.Text, "<@"+sin+">") || !strings.Contains(c.Text, "git.force") {
		t.Fatalf("card: %+v", c)
	}
	time.Sleep(100 * time.Millisecond)
	if tm.fp.count("allow=") != 0 {
		t.Fatal("ran before Sin decided")
	}
	if tm.alpha.Approve(tm.ctx, c.Meta.RequestID, other, true) {
		t.Fatal("a stranger's click counted")
	}
	if tm.alpha.Approve(tm.ctx, c.Meta.RequestID, beta, true) {
		t.Fatal("a partner's click counted")
	}
	if !tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, false) {
		t.Fatal("Sin's click ignored")
	}
	eventually(t, "denied", func() bool { return tm.fp.count("allow=false") == 1 })
	c, _ = card(tm)
	if !c.Updated || !strings.Contains(c.Text, "Denied") {
		t.Fatalf("card not updated: %+v", c)
	}
	if tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, true) {
		t.Fatal("decided twice")
	}
}

func TestApproveByReplyAndOrdinaryActionsPass(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push origin main"})
	eventually(t, "plain push allowed", func() bool { return tm.fp.count("allow=true") == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell rm -rf build"})
	eventually(t, "workdir rm allowed", func() bool { return tm.fp.count("allow=true") == 2 })
	if _, ok := card(tm); ok {
		t.Fatal("ordinary action asked for approval")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell reg add HKCU\\X /v a"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.3", ThreadTS: "1.0", User: other, Text: "批准"})
	time.Sleep(100 * time.Millisecond)
	if tm.fp.count("allow=true") != 2 {
		t.Fatal("stranger's reply approved")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.4", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> 批准"})
	eventually(t, "approved", func() bool { return tm.fp.count("allow=true") == 3 })
}

func TestStopDeniesPendingApproval(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push -f"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "stop"})
	eventually(t, "ack", func() bool { return tm.fp.count(StopAck) == 1 })
	c, _ := card(tm)
	if !strings.Contains(c.Text, "Cancelled by stop") {
		t.Fatalf("%+v", c)
	}
	if a, _, _ := tm.st.GetApproval(c.Meta.RequestID); a.State != store.ApprovalStopped {
		t.Fatal(a.State)
	}
	if tm.fp.count("allow=true") != 0 {
		t.Fatal("ran after stop")
	}
}

func TestPendingApprovalSurvivesRestart(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push --force"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	c, _ := card(tm)
	// "restart": a fresh worker over the same store; the host request is gone
	a := tm.alpha
	tm2 := &team{st: tm.st, fp: tm.fp, ctx: tm.ctx, ha: &fakeHarness{}}
	w2 := &Worker{Bot: a.Bot, Harness: tm2.ha, Store: a.Store, Outbox: a.Outbox, Policy: a.Policy, Owners: a.Owners,
		Peers: a.Peers, Stops: &Stops{}, SelfID: a.SelfID, OriginWait: a.OriginWait}
	w2.Recover(tm.ctx)
	c2, _ := card(tm)
	n := 0
	for _, p := range tm.fp.all() {
		if p.Meta.Kind == "approval" {
			n++
		}
	}
	if n != 1 || !strings.Contains(c2.Text, "Still waiting for Sin") || c2.TS != c.TS {
		t.Fatalf("restart re-posted or did not mark the card: n=%d %+v", n, c2)
	}
	if !w2.Approve(tm.ctx, c.Meta.RequestID, sin, true) {
		t.Fatal("approve after restart")
	}
	// the partner is told it may retry; the retried identical call passes once without a new card
	eventually(t, "retry turn", func() bool {
		for _, tu := range tm2.ha.turns() {
			if strings.Contains(tu.Text, "You may run it once now") {
				return true
			}
		}
		return false
	})
	w2.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push --force"})
	eventually(t, "pre-approved run", func() bool { return tm.fp.count("allow=true") == 1 })
	w2.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.6", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push --force"})
	eventually(t, "second card", func() bool {
		n := 0
		for _, p := range tm.fp.all() {
			if p.Meta.Kind == "approval" {
				n++
			}
		}
		return n == 2
	})
}
