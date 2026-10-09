package slackbot

import (
	"strings"
	"testing"

	"github.com/RCX1t7/plexus/internal/store"
)

func lastPost(tm *team, sub string) posted {
	var p posted
	for _, x := range tm.fp.all() {
		if strings.Contains(x.Text, sub) {
			p = x
		}
	}
	return p
}

// host makes a partner call a host tool: Sin asks it in a fresh message.
func host(t *testing.T, tm *team, w *Worker, ts, call, wantReply string) {
	t.Helper()
	id := alpha
	if w == tm.beta {
		id = beta
	}
	w.Handle(tm.ctx, Inbound{Channel: "C1", TS: ts, ThreadTS: "1.0", User: sin, Text: "<@" + id + "> HOST " + call})
	eventually(t, wantReply, func() bool { return tm.fp.count(wantReply) >= 1 })
	// wait for the turn to end, so the next message starts a turn instead
	// of being steered into this one
	eventually(t, "turn end", func() bool {
		w.mu.Lock()
		th := w.threads["C1:1.0"]
		w.mu.Unlock()
		th.mu.Lock()
		defer th.mu.Unlock()
		return th.cur == nil
	})
}

func nodeState(t *testing.T, tm *team, node string) store.Delegation {
	t.Helper()
	d, ok, _ := tm.st.GetDelegation(node)
	if !ok {
		t.Fatalf("node %s missing", node)
	}
	return d
}

// Review item 8: delegate -> ack -> edit done_when refused -> deliver ->
// reopen -> deliver -> accept, across a restart; owner_if_stuck defaults
// to the delegator; the same reopen reason twice escalates once.
func TestHandoffAckDeliverReviewLifecycle(t *testing.T) {
	tm := newTeam(t, true)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin,
		Text: `<@` + alpha + `> HOST plexus_delegate {"to":"beta","task":"write tests","done_when":"go test passes"}`})
	eventually(t, "card", func() bool { return tm.fp.count("📋 *Handoff*") == 1 })
	card := lastPost(tm, "📋 *Handoff*")
	o, _, _ := tm.st.GetOrigin("C1", card.TS)
	node := o.Handoff
	if d := nodeState(t, tm, node); d.State != store.NodeHanded || d.FromBot != "alpha" || d.ToBot != "beta" || d.OwnerIfStuck != "<@"+alpha+">" {
		t.Fatalf("%+v", d)
	}
	// beta wakes with the node in its prompt
	tm.beta.Handle(tm.ctx, Inbound{Channel: "C1", TS: card.TS, ThreadTS: "1.0", User: alpha, Text: card.Text})
	eventually(t, "beta turn", func() bool { return len(tm.hb.turns()) == 1 })
	if f := tm.hb.turns()[0].Text; !strings.Contains(f, "plexus_ack with this node") || !strings.Contains(f, node) {
		t.Fatal(f)
	}
	// deliver before ack is refused; review by the assignee is refused
	host(t, tm, tm.beta, "2.0", `plexus_deliver {"node":"`+node+`","summary":"early"}`, "host: acknowledge node")
	host(t, tm, tm.beta, "2.1", `plexus_ack {"node":"`+node+`"}`, "host: acknowledged; done_when is locked: go test passes")
	if tm.fp.count("✋ *ACK* node") != 1 {
		t.Fatal("no ACK post")
	}
	host(t, tm, tm.beta, "2.2", `plexus_ack {"node":"`+node+`","done_when":"it compiles"}`, "host: done_when is locked since ACK")
	host(t, tm, tm.beta, "2.3", `plexus_deliver {"node":"`+node+`","summary":"tests","done_when":"it compiles"}`, "host: done_when is locked since ACK")
	host(t, tm, tm.alpha, "2.4", `plexus_review {"node":"`+node+`","decision":"accept"}`, "host: node "+node+" is acked, not delivered")
	host(t, tm, tm.beta, "2.5", `plexus_deliver {"node":"`+node+`","summary":"tests written"}`, "host: delivered node")
	res := lastPost(tm, "📦 *Delivered* — tests written")
	if !strings.HasPrefix(res.Text, "<@"+alpha+">") || !strings.Contains(res.Text, "round 1") {
		t.Fatal(res.Text)
	}
	host(t, tm, tm.beta, "2.6", `plexus_review {"node":"`+node+`","decision":"accept"}`, "host: only the delegator (alpha)")

	// Plexus restarts; the node state survives
	tm.restart()
	// alpha wakes on the delivery with the review instruction
	ro, _, _ := tm.st.GetOrigin("C1", res.TS)
	if ro.Handoff != node {
		t.Fatalf("delivery does not carry the node: %+v", ro)
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: res.TS, ThreadTS: "1.0", User: beta, Text: res.Text})
	eventually(t, "alpha review turn", func() bool { return len(tm.ha.turns()) == 1 })
	if f := tm.ha.turns()[0].Text; !strings.Contains(f, "call plexus_review") {
		t.Fatal(f)
	}
	host(t, tm, tm.alpha, "3.0", `plexus_review {"node":"`+node+`","decision":"reopen","reason":"Flaky test"}`, "host: reopened")
	if p := lastPost(tm, "🔁 *REOPEN*"); !strings.HasPrefix(p.Text, "<@"+beta+">") || !strings.Contains(p.Text, "round 2") {
		t.Fatal(p.Text)
	}
	host(t, tm, tm.beta, "3.1", `plexus_deliver {"node":"`+node+`","summary":"tests fixed"}`, "host: delivered node "+node+" (round 2)")
	// the same reason again: escalation to owner_if_stuck (the delegator itself here)
	host(t, tm, tm.alpha, "3.2", `plexus_review {"node":"`+node+`","decision":"reopen","reason":"flaky test."}`, "you are owner_if_stuck")
	host(t, tm, tm.beta, "3.3", `plexus_deliver {"node":"`+node+`","summary":"tests stable"}`, "host: delivered node "+node+" (round 3)")
	host(t, tm, tm.alpha, "3.4", `plexus_review {"node":"`+node+`","decision":"accept","reason":"green 20x"}`, "host: accepted; node closed")
	if d := nodeState(t, tm, node); d.State != store.NodeAccepted || d.DoneWhen != "go test passes" {
		t.Fatalf("%+v", d)
	}
	host(t, tm, tm.beta, "3.5", `plexus_deliver {"node":"`+node+`","summary":"more"}`, "is already accepted")
}

func TestHandoffSameReasonEscalatesOnceToOwner(t *testing.T) {
	tm := newTeam(t, true)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin,
		Text: `<@` + alpha + `> HOST plexus_delegate {"to":"beta","task":"t","done_when":"d","owner_if_stuck":"<@` + sin + `>"}`})
	eventually(t, "card", func() bool { return tm.fp.count("📋 *Handoff*") == 1 })
	o, _, _ := tm.st.GetOrigin("C1", lastPost(tm, "📋 *Handoff*").TS)
	node := o.Handoff
	host(t, tm, tm.beta, "2.0", `plexus_ack {"node":"`+node+`"}`, "host: acknowledged")
	for i, ts := range []string{"2.1", "2.3", "2.5"} {
		host(t, tm, tm.beta, ts, `plexus_deliver {"node":"`+node+`","summary":"try"}`, "host: delivered node "+node+" (round "+string(rune('1'+i)))
		for _, p := range tm.fp.all() {
			t.Log(p.Text)
		}
		host(t, tm, tm.alpha, ts+"1", `plexus_review {"node":"`+node+`","decision":"reopen","reason":"same bug"}`, "REOPEN* node `"+node+"` (round "+string(rune('2'+i)))
	}
	if n := tm.fp.count("reopened twice for the same reason"); n != 1 {
		t.Fatalf("%d escalations", n)
	}
	if p := lastPost(tm, "reopened twice"); !strings.HasPrefix(p.Text, "<@"+sin+">") {
		t.Fatal(p.Text)
	}
}
