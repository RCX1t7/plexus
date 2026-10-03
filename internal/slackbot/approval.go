package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/danger"
	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/platform"
	"github.com/RCX1t7/plexus/internal/policy"
	"github.com/RCX1t7/plexus/internal/redact"
	"github.com/RCX1t7/plexus/internal/store"
)

// Block Kit action ids of the approval card buttons.
const (
	ActionApprove = "plexus_approve"
	ActionDeny    = "plexus_deny"
)

// approvals holds the live host callbacks waiting for Sin.
type approvals struct {
	mu   sync.Mutex
	live map[string]liveApproval // aid -> callback
}

type liveApproval struct {
	t      *thread
	decide func(harness.Decision)
}

func (a *approvals) add(aid string, l liveApproval) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == nil {
		a.live = map[string]liveApproval{}
	}
	a.live[aid] = l
}

func (a *approvals) take(aid string) (liveApproval, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.live[aid]
	delete(a.live, aid)
	return l, ok
}

func (a *approvals) forThread(t *thread) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id, l := range a.live {
		if l.t == t {
			ids = append(ids, id)
		}
	}
	return ids
}

// gate checks a trusted turn's tool call against the dangerous-action
// rules. It returns true when it took over the callback (an approval card
// was posted and the call waits for Sin, with no timeout).
func (w *Worker) gate(t *thread, j job, ev harness.Event, d harness.Decision) bool {
	if !d.Allow || w.Policy.Restricted(j.auth) || ev.Perm == nil {
		return false
	}
	tool := ev.Perm.Tool
	call := danger.Call{Kind: tool.Kind, Name: tool.Name, Command: tool.Command, Paths: tool.Paths}
	hit := danger.Classify(call, danger.Ctx{GOOS: w.Policy.GOOS, Workdir: w.Policy.Workdir,
		HeadPushed: func() bool { return headPushed(w.Policy.Workdir) }}, w.Danger)
	if hit == nil {
		return false
	}
	fp := danger.Fingerprint(call)
	if ok, _ := w.Store.ConsumePreApproval(w.Bot.Name, t.key, fp); ok {
		w.log().Info("dangerous action pre-approved by Sin", "rule", hit.Rule)
		return false
	}
	w.mu.Lock()
	w.seq++
	n := w.seq
	w.mu.Unlock()
	aid := RequestID(w.Bot.Name, t.key, "approval", j.in.TS, firstNonEmpty(tool.CallID, ev.ID), fmt.Sprint(n))
	summary := tool.Command
	if summary == "" {
		summary = strings.TrimSpace(tool.Name + " " + strings.Join(tool.Paths, " "))
	}
	summary = redact.String(summary)
	if len(summary) > 600 {
		summary = summary[:600] + "…"
	}
	a := store.Approval{AID: aid, Bot: w.Bot.Name, Thread: t.key, Channel: t.channel, ThreadTS: t.ts, Root: j.auth.Root,
		Rule: hit.Rule, Reason: hit.Reason, CallFP: fp, Summary: summary, CardID: aid, State: store.ApprovalPending}
	if err := w.Store.PutApproval(a); err != nil {
		ev.Decide(harness.Decision{Allow: false, Reason: "could not record the approval request"})
		return true
	}
	w.approvals.add(aid, liveApproval{t: t, decide: ev.Decide})
	text, blocks := w.approvalCard(a)
	ids, err := w.Outbox.Enqueue(Post{ID: aid, Channel: t.channel, Thread: t.ts, Text: text, Kind: "approval",
		Origin: string(j.auth.Source), Blocks: blocks})
	if err == nil {
		for _, id := range ids {
			_, _ = w.Outbox.Deliver(context.Background(), id)
		}
	}
	w.log().Warn("dangerous action waits for Sin", "rule", hit.Rule, "aid", aid)
	return true
}

func (w *Worker) approvalCard(a store.Approval) (string, json.RawMessage) {
	who := "Sin"
	if len(w.Owners) > 0 {
		who = "<@" + w.Owners[0] + ">"
	}
	text := fmt.Sprintf("%s ⚠️ *%s wants to do something that needs your approval* (`%s`)\n> %s\n```%s```\n"+
		"Only Sin's click counts. You can also reply `approve` / `批准` or `deny` / `拒绝` in this thread.",
		who, w.Bot.Name, a.Rule, a.Reason, strings.ReplaceAll(a.Summary, "```", "'''"))
	blocks := []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "actions", "elements": []any{
			map[string]any{"type": "button", "style": "primary", "action_id": ActionApprove, "value": a.AID,
				"text": map[string]any{"type": "plain_text", "text": "Approve / 批准"}},
			map[string]any{"type": "button", "style": "danger", "action_id": ActionDeny, "value": a.AID,
				"text": map[string]any{"type": "plain_text", "text": "Deny / 拒绝"}},
		}},
	}
	b, _ := json.Marshal(blocks)
	return text, b
}

// Approve applies Sin's decision on an approval card. Clicks or replies by
// anyone else are ignored. It reports whether the decision was taken.
func (w *Worker) Approve(ctx context.Context, aid, user string, ok bool) bool {
	if !w.isOwner(user) {
		w.log().Info("approval click ignored: not Sin", "user", user)
		return false
	}
	state := store.ApprovalDenied
	if ok {
		state = store.ApprovalApproved
	}
	a, changed, err := w.Store.DecideApproval(aid, state, user)
	if err != nil || !changed || a.Bot != w.Bot.Name {
		return false
	}
	verdict := "❌ Denied by <@" + user + ">"
	if ok {
		verdict = "✅ Approved by <@" + user + "> (this one call only)"
	}
	_ = w.Outbox.Update(ctx, a.CardID, fmt.Sprintf("%s — `%s`\n```%s```", verdict, a.Rule, a.Summary))
	if l, live := w.approvals.take(aid); live {
		if ok {
			a.State = store.ApprovalConsumed // used right away by the waiting call
			_ = w.Store.PutApproval(a)
			l.decide(harness.Decision{Allow: true, Reason: "approved by Sin"})
		} else {
			l.decide(harness.Decision{Allow: false, Reason: "Sin denied this action; do not retry it, continue with other work or ask Sin"})
		}
		return true
	}
	// The host request died with a restart: an approval becomes a one-time
	// pre-approval for the same call; tell the partner it may retry.
	if ok {
		key := a.Thread
		in := Inbound{Channel: a.Channel, ThreadTS: a.ThreadTS, TS: a.ThreadTS, User: user}
		t := w.thread(ctx, key, in)
		t.push(ctx, job{in: Inbound{Channel: a.Channel, ThreadTS: a.ThreadTS, TS: a.AID, User: user,
			Text: "Sin approved the action you requested before the restart (" + a.Summary + "). You may run it once now."},
			auth: policy.Authority{Source: policy.FromSin, Root: a.Root}})
	}
	return true
}

// approvalReply handles Sin's text answer to the newest pending approval
// of this thread.
func (w *Worker) approvalReply(ctx context.Context, key string, in Inbound) bool {
	l := strings.ToLower(strings.TrimSpace(in.Text))
	var ok bool
	switch l {
	case "approve", "approved", "批准", "同意":
		ok = true
	case "deny", "denied", "拒绝":
	default:
		return false
	}
	pending, _ := w.Store.Approvals(w.Bot.Name, store.ApprovalPending)
	var newest *store.Approval
	for i := range pending {
		if pending[i].Thread == key && (newest == nil || pending[i].Created > newest.Created) {
			newest = &pending[i]
		}
	}
	if newest == nil {
		return false
	}
	return w.Approve(ctx, newest.AID, in.User, ok)
}

// cancelApprovals denies everything thread t waits for (stop).
func (w *Worker) cancelApprovals(t *thread) {
	for _, aid := range w.approvals.forThread(t) {
		a, changed, _ := w.Store.DecideApproval(aid, store.ApprovalStopped, "stop")
		if l, ok := w.approvals.take(aid); ok {
			l.decide(harness.Decision{Allow: false, Reason: "the task was stopped"})
		}
		if changed {
			_ = w.Outbox.Update(context.Background(), a.CardID, "🛑 Cancelled by stop — `"+a.Rule+"`")
		}
	}
}

// recoverApprovals marks cards of approvals that survived a restart.
func (w *Worker) recoverApprovals(ctx context.Context) {
	pending, _ := w.Store.Approvals(w.Bot.Name, store.ApprovalPending)
	for _, a := range pending {
		if stopped, _ := w.Store.AnyRevoked(a.Root); stopped {
			_, _, _ = w.Store.DecideApproval(a.AID, store.ApprovalStopped, "stop")
			_ = w.Outbox.Update(ctx, a.CardID, "🛑 Cancelled by stop — `"+a.Rule+"`")
			continue
		}
		_ = w.Outbox.Update(ctx, a.CardID, fmt.Sprintf("⏳ *Still waiting for Sin* (Plexus restarted) — `%s`\n```%s```\n"+
			"Reply `approve` / `批准` or `deny` / `拒绝` here; approving lets %s run it once when it retries.", a.Rule, a.Summary, w.Bot.Name))
	}
}

// headPushed reports whether HEAD is already contained in its upstream.
func headPushed(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "merge-base", "--is-ancestor", "HEAD", "@{u}")
	platform.HideWindow(cmd)
	return cmd.Run() == nil
}
