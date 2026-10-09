package slackbot

import (
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/store"
)

func cardsList(tm *team) []posted {
	var out []posted
	for _, p := range tm.fp.all() {
		if p.Meta.Kind == "approval" {
			out = append(out, p)
		}
	}
	return out
}

// hasButtons reports whether a card still offers all three actions.
func hasButtons(p posted) bool {
	b := string(p.Meta.Blocks)
	return strings.Contains(b, `"action_id":"`+ActionApprove+`"`) && strings.Contains(b, `"action_id":"`+ActionApproveTask+`"`) &&
		strings.Contains(b, `"action_id":"`+ActionDeny+`"`)
}

func TestCardHasThreeButtons(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push -f origin main"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	c, _ := card(tm)
	b := string(c.Meta.Blocks)
	for _, s := range []string{ActionApprove, ActionApproveTask, ActionDeny, "仅此一次", "本任务内批准", "拒绝"} {
		if !strings.Contains(b, s) {
			t.Fatalf("card lacks %q: %s", s, b)
		}
	}
	if ModeForAction(ActionApproveTask) != DecideTask || ModeForAction("x") != "" {
		t.Fatal("action mapping")
	}
}

// Review item 1: "approve for this task" grants (rule, target) to every
// partner in the tree, merges cards by (rule, target), survives a restart
// and does not leak into another tree.
func TestApproveForTaskGrant(t *testing.T) {
	tm := newTeam(t, false)
	push := " TOOL shell git push -f https://github.com/o/r main"
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + ">" + push})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	// beta joins the same tree and hits the same (rule, target): merged, no second card
	tm.beta.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + beta + ">" + push})
	eventually(t, "beta parked", func() bool { return tm.fp.count("allow=false 已暂挂") == 2 })
	eventually(t, "card shows merge", func() bool { c, _ := card(tm); return strings.Contains(c.Text, "1 more call(s) from beta") })
	if n := len(cardsList(tm)); n != 1 {
		t.Fatalf("%d cards for one (rule, target)", n)
	}
	c, _ := card(tm)
	if !c.Updated || !hasButtons(c) {
		t.Fatalf("merged card lost its buttons: %s", c.Meta.Blocks)
	}
	if !tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, DecideTask) {
		t.Fatal("task approval not taken")
	}
	eventually(t, "both told", func() bool {
		return turnSays(tm.ha, "for this whole task")() && turnSays(tm.hb, "for this whole task")()
	})
	if g, _ := tm.st.Grants("C1:1.0"); len(g) != 1 || g[0].Rule != "git.force" || !strings.HasSuffix(g[0].Target, "#main") {
		t.Fatalf("grants %+v", g)
	}
	before := tm.fp.count("allow=true")
	// the same push again, by either partner in the tree: not parked
	tm.beta.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: sin, Text: "<@" + beta + ">" + push})
	eventually(t, "granted run", func() bool { return tm.fp.count("allow=true") >= before+1 })
	// "restart": fresh workers over the same store still honor the grant
	w2 := &Worker{Bot: tm.alpha.Bot, Harness: &fakeHarness{}, Store: tm.st, Outbox: tm.alpha.Outbox, Policy: tm.alpha.Policy,
		Owners: tm.alpha.Owners, Peers: tm.peers, Stops: &Stops{}, SelfID: alpha, OriginWait: tm.alpha.OriginWait}
	w2.Recover(tm.ctx)
	n := tm.fp.count("allow=true")
	w2.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.6", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + ">" + push})
	eventually(t, "grant after restart", func() bool { return tm.fp.count("allow=true") == n+1 })
	// another tree: parked again
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C2", TS: "2.0", User: sin, Text: "<@" + alpha + ">" + push})
	eventually(t, "other tree parked", func() bool { return len(cardsList(tm)) == 2 })
	// stop drops the grant
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.9", ThreadTS: "1.0", User: sin, Text: "stop"})
	eventually(t, "grant dropped", func() bool { g, _ := tm.st.Grants("C1:1.0"); return len(g) == 0 })
	eventually(t, "stop done", func() bool { return tm.fp.count(StopAck) == 1 })
}

func TestApproveOnceDoesNotGrant(t *testing.T) {
	tm := newTeam(t, false)
	push := "<@" + alpha + "> TOOL shell git push -f origin main"
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: push})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> 仅此一次"})
	eventually(t, "told", turnSays(tm.ha, "it will be allowed once"))
	if g, _ := tm.st.Grants("C1:1.0"); len(g) != 0 {
		t.Fatal("once created a grant")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: push})
	eventually(t, "runs once", func() bool { return tm.fp.count("allow=true") == 1 })
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.3", ThreadTS: "1.0", User: sin, Text: push})
	eventually(t, "parked again", func() bool { return len(cardsList(tm)) == 2 })
	// a text reply can grant for the task too
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.4", ThreadTS: "1.0", User: sin, Text: "本任务内批准"})
	eventually(t, "grant by reply", func() bool { g, _ := tm.st.Grants("C1:1.0"); return len(g) == 1 })
}

// Review item 2: after a restart, a stop cancels the approvals parked
// before it and updates their cards; a later click is a no-op.
func TestStopAfterRestartCancelsApprovals(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOL shell git push -f origin main"})
	eventually(t, "card", func() bool { _, ok := card(tm); return ok })
	c, _ := card(tm)
	eventually(t, "card recorded as sent", func() bool { m, _ := tm.st.Get(c.Meta.RequestID); return m.SlackTS != "" })
	// restart: new workers and a new (empty) live registry over the same store
	stops := &Stops{}
	mk := func(old *Worker) *Worker {
		return &Worker{Bot: old.Bot, Harness: &fakeHarness{}, Store: tm.st, Outbox: old.Outbox, Policy: old.Policy,
			Owners: old.Owners, Peers: tm.peers, Stops: stops, SelfID: old.SelfID, OriginWait: old.OriginWait}
	}
	a2, b2 := mk(tm.alpha), mk(tm.beta)
	a2.Recover(tm.ctx)
	b2.Recover(tm.ctx)
	a2.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "stop"})
	eventually(t, "ack", func() bool { return tm.fp.count(StopAck) == 1 })
	eventually(t, "cancelled", func() bool {
		a, _, _ := tm.st.GetApproval(c.Meta.RequestID)
		return a.State == store.ApprovalCancelled
	})
	c, _ = card(tm)
	if !c.Updated || !strings.Contains(c.Text, "已随 stop 取消") {
		t.Fatalf("card not updated: %+v", c)
	}
	if a2.Approve(tm.ctx, c.Meta.RequestID, sin, DecideOnce) {
		t.Fatal("click on a cancelled card counted")
	}
	time.Sleep(50 * time.Millisecond)
	if a, _, _ := tm.st.GetApproval(c.Meta.RequestID); a.State != store.ApprovalCancelled {
		t.Fatal(a.State)
	}
}

// B2: "仅此一次" lets only the identical call through: an MCP send to
// another recipient, or the same rm in another workdir, parks again.
func TestApproveOnceOnlyIdenticalCall(t *testing.T) {
	tm := newTeam(t, false)
	ask := func(ts, call string) {
		tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: ts, ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> TOOLJ " + call})
	}
	sendA := `mcp__slack__send_message {"channel":"C_SIN","text":"report"}`
	sendB := `mcp__slack__send_message {"channel":"C_PUBLIC","text":"report"}`
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> TOOLJ " + sendA})
	eventually(t, "card", func() bool { return len(cardsList(tm)) == 1 })
	c, _ := card(tm)
	if !tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, DecideOnce) {
		t.Fatal("approve once")
	}
	ask("1.1", sendB) // another recipient: parked, not the approved call
	eventually(t, "B parked", func() bool { return len(cardsList(tm)) == 2 })
	if tm.fp.count("allow=true") != 0 {
		t.Fatal("approve-once let a send to another recipient through")
	}
	ask("1.2", sendA) // the identical call: runs once
	eventually(t, "A runs", func() bool { return tm.fp.count("allow=true") == 1 })

	rmTmp := `bash {"command":"rm -rf build","workdir":"/tmp/p1"}`
	rmOther := `bash {"command":"rm -rf build","workdir":"/tmp/p2"}`
	ask("2.0", rmTmp)
	eventually(t, "rm card", func() bool { return len(cardsList(tm)) == 3 })
	c = cardsList(tm)[2]
	if !tm.alpha.Approve(tm.ctx, c.Meta.RequestID, sin, DecideOnce) {
		t.Fatal("approve once rm")
	}
	ask("2.1", rmOther) // same command, another workdir: parked
	eventually(t, "other workdir parked", func() bool { return len(cardsList(tm)) == 4 })
	if tm.fp.count("allow=true") != 1 {
		t.Fatal("approve-once let the rm in another workdir through")
	}
	ask("2.2", rmTmp)
	eventually(t, "identical rm runs", func() bool { return tm.fp.count("allow=true") == 2 })
}
