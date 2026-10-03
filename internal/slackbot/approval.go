package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
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

// Parked is the reason a dangerous call is denied with while it waits for
// Sin. The callback never blocks: the call is denied at once and parked;
// once Sin approves, the identical re-issued call is allowed (once).
const Parked = "已暂挂，等 Sin 批准 (parked: this action needs Sin's approval in the Slack thread). " +
	"Do not retry it now; continue with other work. Plexus will tell you when Sin decides; " +
	"if Sin approves, re-issue exactly the same call and it will be allowed once."

// gate checks a trusted turn's tool call against the dangerous-action
// rules. It returns true when it decided the call itself: a dangerous call
// is denied at once and parked behind an approval card (one card per
// distinct call), unless Sin already approved this exact call.
func (w *Worker) gate(t *thread, j job, ev harness.Event, d harness.Decision) bool {
	if !d.Allow || w.Policy.Restricted(j.auth) || ev.Perm == nil {
		return false
	}
	tool := harness.Normalize(ev.Perm.Tool) // idempotent: fills what the adapter left out
	call := danger.Call{Kind: tool.Kind, Name: tool.Name, Command: tool.Command, Paths: tool.Paths, Deletes: tool.Deletes, Input: tool.Input}
	hit := danger.Classify(call, danger.Ctx{GOOS: w.Policy.GOOS, Workdir: w.Policy.Workdir, Git: runGit}, w.Danger)
	if hit == nil {
		return false
	}
	fp := danger.Fingerprint(call)
	if ok, _ := w.Store.ConsumePreApproval(w.Bot.Name, t.key, fp); ok {
		w.log().Info("dangerous action approved by Sin; allowing the re-issued call once", "rule", hit.Rule)
		return false
	}
	ev.Decide(harness.Decision{Allow: false, Reason: Parked})
	pending, _ := w.Store.Approvals(w.Bot.Name, store.ApprovalPending)
	for _, a := range pending {
		if a.Thread == t.key && a.CallFP == fp {
			w.log().Info("dangerous action still parked", "rule", hit.Rule, "aid", a.AID)
			return true // the card is already there
		}
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
		w.log().Error("could not record an approval request", "err", err.Error())
		return true
	}
	text, blocks := w.approvalCard(a)
	ids, err := w.Outbox.Enqueue(Post{ID: aid, Channel: t.channel, Thread: t.ts, Text: text, Kind: "approval",
		Origin: string(j.auth.Source), Blocks: blocks})
	if err == nil {
		for _, id := range ids {
			_, _ = w.Outbox.Deliver(context.Background(), id)
		}
	}
	w.log().Warn("dangerous action parked until Sin decides", "rule", hit.Rule, "aid", aid)
	return true
}

func (w *Worker) approvalCard(a store.Approval) (string, json.RawMessage) {
	who := "Sin"
	if len(w.Owners) > 0 {
		who = "<@" + w.Owners[0] + ">"
	}
	text := fmt.Sprintf("%s ⚠️ *%s wants to do something that needs your approval* (`%s`) — 已暂挂，等 Sin 批准\n> %s\n```%s```\n"+
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
// anyone else are ignored. It reports whether the decision was taken. The
// partner is told the outcome in a new turn; after an approval it may
// re-issue the identical call, which then passes once.
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
	if stopped, _ := w.Store.AnyRevoked(a.Root); stopped {
		return true // decided, but the task is stopped: nothing to resume
	}
	verdict := "❌ Denied by <@" + user + ">"
	note := "Sin denied the parked action (" + a.Summary + "). Do not run it; continue with other work or ask Sin."
	if ok {
		verdict = "✅ Approved by <@" + user + "> (this one call only)"
		note = "Sin approved the parked action (" + a.Summary + "). Re-issue exactly the same call now; it will be allowed once."
	}
	_ = w.Outbox.Update(ctx, a.CardID, fmt.Sprintf("%s — `%s`\n```%s```", verdict, a.Rule, a.Summary))
	in := Inbound{Channel: a.Channel, ThreadTS: a.ThreadTS, TS: a.ThreadTS, User: user}
	t := w.thread(ctx, a.Thread, in)
	t.push(ctx, job{in: Inbound{Channel: a.Channel, ThreadTS: a.ThreadTS, TS: a.AID, User: user, Text: note},
		auth: policy.Authority{Source: policy.FromSin, Root: a.Root}})
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

// cancelApprovals ends every parked or approved-but-unused approval of
// thread t (stop).
func (w *Worker) cancelApprovals(t *thread) {
	for _, st := range []string{store.ApprovalPending, store.ApprovalApproved} {
		list, _ := w.Store.Approvals(w.Bot.Name, st)
		for _, a := range list {
			if a.Thread != t.key {
				continue
			}
			if _, changed, _ := w.Store.DecideApproval(a.AID, store.ApprovalStopped, "stop"); changed {
				_ = w.Outbox.Update(context.Background(), a.CardID, "🛑 Cancelled by stop — `"+a.Rule+"`")
			}
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
		_ = w.Outbox.Update(ctx, a.CardID, fmt.Sprintf("⏳ *Still waiting for Sin* (Plexus restarted) — 已暂挂，等 Sin 批准 — `%s`\n```%s```\n"+
			"Click below or reply `approve` / `批准` or `deny` / `拒绝` here; approving lets %s run it once when it re-issues the call.", a.Rule, a.Summary, w.Bot.Name))
	}
}

// runGit runs a read-only git query in dir for the classifier (targets,
// "is HEAD pushed"). It only runs for calls that are already git rules.
func runGit(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	platform.HideWindow(cmd)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err == nil
}
