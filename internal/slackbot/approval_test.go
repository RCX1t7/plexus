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

func cards(tm *team) int {
	n := 0
	for _, p := range tm.fp.all() {
		if p.Meta.Kind == "approval" {
			n++
		}
	}
	return n
}

func turnSays(h *fakeHarness, s string) func() bool {
	return func() bool {
		for _, tu := range h.turns() {
			if strings.Contains(tu.Text, s) {
				return true
			}
		}
		return false
	}
}

func TestDangerousActionIsParkedForSinOnly(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push --force origin main"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	c, _ := card(tm)
	if len(c.Meta.Blocks) == 0 || !strings.Contains(c.Text, "<@"+sin+">") || !strings.Contains(c.Text, "git.force") {
		t.Fatalf("card: %+v", c)
	}
	// the callback is denied at once, with the parked reason
	eventually(t, "parked", func() bool { return tm.fp.count("allow=false 已暂挂，等 Sin 批准") == 1 })
	if tm.fp.count("allow=true") != 0 {
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
	eventually(t, "partner told", turnSays(tm.ha, "Sin denied the parked action"))
	c, _ = card(tm)
	if !c.Updated || !strings.Contains(c.Text, "Denied") {
		t.Fatalf("card not updated: %+v", c)
	}
	if tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, true) {
		t.Fatal("decided twice")
	}
}

func TestApproveByReplyThenReissuedCallPassesOnce(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push origin main"})
	eventually(t, "plain push allowed", func() bool { return tm.fp.count("allow=true") == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell rm -rf build"})
	eventually(t, "workdir rm allowed", func() bool { return tm.fp.count("allow=true") == 2 })
	if _, ok := card(tm); ok {
		t.Fatal("ordinary action asked for approval")
	}
	reg := "<@" + alpha + "> TOOL shell reg add HKCU\\X /v a"
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: reg})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	// re-issuing before Sin decides is parked again without a second card
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.3", ThreadTS: "1.0", User: sin, Text: reg})
	eventually(t, "parked twice", func() bool { return tm.fp.count("已暂挂") >= 3 }) // card + 2 denials
	if cards(tm) != 1 {
		t.Fatal("second card for the same parked call")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.4", ThreadTS: "1.0", User: other, Text: "批准"})
	time.Sleep(100 * time.Millisecond)
	if a, _, _ := tm.st.GetApproval(firstCardID(tm)); a.State != store.ApprovalPending {
		t.Fatal("stranger's reply decided")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> 批准"})
	eventually(t, "partner told", turnSays(tm.ha, "Re-issue exactly the same call"))
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.6", ThreadTS: "1.0", User: sin, Text: reg})
	eventually(t, "approved call runs", func() bool { return tm.fp.count("allow=true") == 3 })
	// only once
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.7", ThreadTS: "1.0", User: sin, Text: reg})
	eventually(t, "new card", func() bool { return cards(tm) == 2 })
}

func firstCardID(tm *team) string {
	c, _ := card(tm)
	return c.Meta.RequestID
}

func TestStopCancelsParkedApproval(t *testing.T) {
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
	if tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, true) {
		t.Fatal("approved after stop")
	}
	if tm.fp.count("allow=true") != 0 {
		t.Fatal("ran after stop")
	}
}

func TestParkedApprovalSurvivesRestart(t *testing.T) {
	tm := newTeam(t, false)
	push := "<@" + alpha + "> TOOL shell git push --force"
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: push})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	c, _ := card(tm)
	// "restart": a fresh worker over the same store
	a := tm.alpha
	h2 := &fakeHarness{}
	w2 := &Worker{Bot: a.Bot, Harness: h2, Store: a.Store, Outbox: a.Outbox, Policy: a.Policy, Owners: a.Owners,
		Peers: a.Peers, Stops: &Stops{}, SelfID: a.SelfID, OriginWait: a.OriginWait}
	w2.Recover(tm.ctx)
	c2, _ := card(tm)
	if cards(tm) != 1 || !strings.Contains(c2.Text, "Still waiting for Sin") || c2.TS != c.TS {
		t.Fatalf("restart re-posted or did not mark the card: n=%d %+v", cards(tm), c2)
	}
	if !w2.Approve(tm.ctx, c.Meta.RequestID, sin, true) {
		t.Fatal("approve after restart")
	}
	eventually(t, "partner told", turnSays(h2, "Re-issue exactly the same call"))
	w2.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: sin, Text: push})
	eventually(t, "approved run", func() bool { return tm.fp.count("allow=true") == 1 })
}
