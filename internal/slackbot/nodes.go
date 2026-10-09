package slackbot

import (
	"errors"
	"fmt"
	"strings"

	"github.com/RCX1t7/plexus/internal/handoff"
	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/store"
)

// The minimal handoff path (review #8): HANDOFF -> ACK -> RESULT ->
// ACCEPT / REOPEN. Node state lives in bbolt ("delegations"), so it
// survives restarts; the posts carry the node, so the partner they
// mention wakes with the node in its prompt.

type nodeArgs struct {
	Node     string `json:"node"`
	DoneWhen any    `json:"done_when"` // string or list of strings
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (a nodeArgs) doneWhen() string {
	switch v := a.DoneWhen.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var p []string
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				p = append(p, strings.TrimSpace(s))
			}
		}
		return strings.Join(p, "; ")
	}
	return ""
}

func nodeErr(f string, a ...any) harness.HostResult {
	return harness.HostResult{Text: fmt.Sprintf(f, a...), IsError: true}
}

var errNode = errors.New("node")

func (w *Worker) ackNode(t *thread, j job, callID string, a nodeArgs) harness.HostResult {
	dw := a.doneWhen()
	var why string
	refuse := func(f string, v ...any) error { why = fmt.Sprintf(f, v...); return errNode }
	acked := false
	d, err := w.Store.UpdateDelegation(a.Node, func(d *store.Delegation) error {
		if d.ToBot != w.Bot.Name {
			return refuse("node %s is not handed to you", a.Node)
		}
		if d.State != store.NodeHanded {
			if dw != "" && dw != d.DoneWhen {
				return refuse("done_when is locked since ACK: %q", d.DoneWhen)
			}
			return nil // idempotent
		}
		if dw != "" {
			d.DoneWhen = dw
		}
		if d.DoneWhen == "" {
			return refuse("done_when is required: state it before starting")
		}
		d.State, acked = store.NodeAcked, true
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nodeErr("unknown node %s", a.Node)
	}
	if err != nil {
		return nodeErr("%s", firstNonEmptyStr(why, fmt.Sprint(err)))
	}
	if !acked {
		return harness.HostResult{Text: "already acknowledged; done_when: " + d.DoneWhen}
	}
	w.postCarrying(t, j, RequestID(w.Bot.Name, "ack", a.Node), "ack", fmt.Sprintf("✋ *ACK* node `%s` · done when: %s", a.Node, d.DoneWhen), a.Node)
	return harness.HostResult{Text: "acknowledged; done_when is locked: " + d.DoneWhen}
}

func (w *Worker) deliverNode(t *thread, j job, callID string, a nodeArgs, card string) harness.HostResult {
	dw := a.doneWhen()
	var why string
	refuse := func(f string, v ...any) error { why = fmt.Sprintf(f, v...); return errNode }
	d, err := w.Store.UpdateDelegation(a.Node, func(d *store.Delegation) error {
		switch {
		case d.ToBot != w.Bot.Name:
			return refuse("node %s is not handed to you", a.Node)
		case dw != "" && dw != d.DoneWhen:
			return refuse("done_when is locked since ACK: %q", d.DoneWhen)
		case d.State == store.NodeHanded:
			return refuse("acknowledge node %s with plexus_ack first", a.Node)
		case d.State == store.NodeDelivered:
			return refuse("node %s is already delivered and waits for review", a.Node)
		case d.State == store.NodeAccepted:
			return refuse("node %s is already accepted", a.Node)
		}
		d.State = store.NodeDelivered
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nodeErr("unknown node %s", a.Node)
	}
	if err != nil {
		return nodeErr("%s", firstNonEmptyStr(why, fmt.Sprint(err)))
	}
	to := w.Peers.ID(d.FromBot)
	text := fmt.Sprintf("<@%s> %s\n> node `%s` · round %d · done when: %s\n> review it with plexus_review (accept / reopen)", to, card, a.Node, d.Round, d.DoneWhen)
	w.postCarrying(t, j, RequestID(w.Bot.Name, "deliver", a.Node, fmt.Sprint(d.Round)), "deliver", text, a.Node)
	return harness.HostResult{Text: fmt.Sprintf("delivered node %s (round %d); %s reviews it", a.Node, d.Round, d.FromBot)}
}

// normReason folds a reopen reason for the same-error rule.
func normReason(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.Trim(s, " .!。"))), " ")
}

func (w *Worker) reviewNode(t *thread, j job, callID string, a nodeArgs) harness.HostResult {
	dec := strings.ToLower(strings.TrimSpace(a.Decision))
	if dec != "accept" && dec != "reopen" {
		return nodeErr("decision must be accept or reopen")
	}
	reason := strings.TrimSpace(a.Reason)
	if dec == "reopen" && reason == "" {
		return nodeErr("a reopen needs a reason")
	}
	var why string
	refuse := func(f string, v ...any) error { why = fmt.Sprintf(f, v...); return errNode }
	escalate := false
	var round int
	d, err := w.Store.UpdateDelegation(a.Node, func(d *store.Delegation) error {
		if d.FromBot != w.Bot.Name {
			return refuse("only the delegator (%s) reviews node %s", d.FromBot, a.Node)
		}
		if d.State != store.NodeDelivered {
			return refuse("node %s is %s, not delivered", a.Node, d.State)
		}
		round = d.Round
		if dec == "accept" {
			d.State = store.NodeAccepted
			return nil
		}
		n := normReason(reason)
		escalate = d.LastReason != "" && n == d.LastReason && !d.Escalated
		d.State, d.LastReason, d.Round = store.NodeReopened, n, d.Round+1
		d.Escalated = d.Escalated || escalate
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nodeErr("unknown node %s", a.Node)
	}
	if err != nil {
		return nodeErr("%s", firstNonEmptyStr(why, fmt.Sprint(err)))
	}
	id := RequestID(w.Bot.Name, "review", a.Node, fmt.Sprint(round))
	if dec == "accept" {
		w.postCarrying(t, j, id, "review", fmt.Sprintf("✅ *ACCEPT* node `%s`%s", a.Node, suffix(" — ", reason)), a.Node)
		return harness.HostResult{Text: "accepted; node closed"}
	}
	w.postCarrying(t, j, id, "review", fmt.Sprintf("<@%s> 🔁 *REOPEN* node `%s` (round %d): %s\n> done when: %s",
		w.Peers.ID(d.ToBot), a.Node, d.Round, reason, d.DoneWhen), a.Node)
	if escalate && d.OwnerIfStuck != "" && d.OwnerIfStuck != "<@"+w.SelfID+">" {
		w.postCarrying(t, j, id+"-stuck", "review", fmt.Sprintf("%s ⚠️ node `%s` was reopened twice for the same reason: %s\nPlease decide how to proceed.",
			d.OwnerIfStuck, a.Node, reason), a.Node)
		return harness.HostResult{Text: "reopened; same reason twice, escalated to " + d.OwnerIfStuck}
	}
	if escalate {
		return harness.HostResult{Text: "reopened; this is the second reopen for the same reason and you are owner_if_stuck: change the approach or ask Sin"}
	}
	return harness.HostResult{Text: "reopened"}
}

func suffix(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// nodePrompt tells the receiving harness what the carried record/node is.
func (w *Worker) nodePrompt(id string) string {
	h, ok, _ := w.Store.GetHandoff(id)
	if !ok {
		return ""
	}
	var r handoff.Record
	if jsonUnmarshal(h.Record, &r) != nil {
		return ""
	}
	d, ok, _ := w.Store.GetDelegation(id)
	if !ok {
		return r.Prompt("") + "\n"
	}
	if d.DoneWhen != "" {
		r.DoneWhen = d.DoneWhen
	}
	switch {
	case d.FromBot == w.Bot.Name && d.State == store.NodeDelivered:
		return fmt.Sprintf("[Delivery of node `%s` you delegated to %s (round %d). done_when: %s. Check the delivery against done_when, then call plexus_review with accept or reopen and a reason.]\n",
			id, d.ToBot, d.Round, d.DoneWhen)
	case d.FromBot == w.Bot.Name:
		return fmt.Sprintf("[Node `%s` you delegated to %s is %s.]\n", id, d.ToBot, d.State)
	case d.ToBot == w.Bot.Name && d.State == store.NodeReopened:
		return r.Prompt("") + fmt.Sprintf("\n[Node `%s` was reopened (round %d). done_when is locked: %s. Fix it, then plexus_deliver with this node.]\n", id, d.Round, d.DoneWhen)
	case d.ToBot == w.Bot.Name && d.State != store.NodeHanded:
		return r.Prompt("") + fmt.Sprintf("\n[Node `%s` is %s; done_when is locked.]\n", id, d.State)
	}
	return r.Prompt(id) + "\n"
}
