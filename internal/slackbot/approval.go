package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
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
	ActionApprove     = "plexus_approve"      // 仅此一次: the identical re-issued call passes once
	ActionApproveTask = "plexus_approve_task" // 本任务内批准: grant (rule, target) for the task tree
	ActionDeny        = "plexus_deny"
)

// Decision modes for Approve.
const (
	DecideOnce = store.ModeOnce
	DecideTask = store.ModeTask
	DecideDeny = "deny"
)

// ModeForAction maps a card button to a decision mode ("" if not ours).
func ModeForAction(actionID string) string {
	switch actionID {
	case ActionApprove:
		return DecideOnce
	case ActionApproveTask:
		return DecideTask
	case ActionDeny:
		return DecideDeny
	}
	return ""
}

// CancelledByStop is how a card reads once its task tree is stopped.
const CancelledByStop = "🛑 已随 stop 取消 / cancelled by stop"

// Parked is the reason a dangerous call is denied with while it waits for
// Sin. The callback never blocks: the call is denied at once and parked;
// once Sin approves, the identical re-issued call is allowed (once).
const Parked = "已暂挂，等 Sin 批准 (parked: this action needs Sin's approval in the Slack thread). " +
	"Do not retry it now; continue with other work. Plexus will tell you when Sin decides; " +
	"if Sin approves, re-issue exactly the same call and it will be allowed once."

// gate checks a trusted turn's tool call against the dangerous-action
// rules. It returns true when it decided the call itself: a dangerous call
// is denied at once and parked behind an approval card, unless Sin granted
// its (rule, target) for this task tree or approved this exact call once.
// A second dangerous call with the same (rule, target) anywhere in the tree
// merges into the first card instead of posting another.
func (w *Worker) gate(t *thread, j job, ev harness.Event, d harness.Decision) bool {
	if !d.Allow || w.Policy.Restricted(j.auth) || ev.Perm == nil {
		return false
	}
	tool := harness.Normalize(ev.Perm.Tool) // idempotent: fills what the adapter left out
	call := danger.Call{Kind: tool.Kind, Name: tool.Name, Command: tool.Command, Paths: tool.Paths, Deletes: tool.Deletes, Input: tool.Input, Workdir: tool.Workdir}
	hit := danger.Classify(call, danger.Ctx{GOOS: w.Policy.GOOS, Workdir: w.Policy.Workdir, Git: runGit}, w.Danger)
	if hit == nil {
		return false
	}
	root := j.auth.Root
	if ok, _ := w.Store.GrantCovers(root, hit.Rule, hit.Target); ok {
		w.log().Info("dangerous action covered by Sin's grant for this task", "rule", hit.Rule, "target", hit.Target)
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
	a := store.Approval{AID: aid, Bot: w.Bot.Name, Thread: t.key, Channel: t.channel, ThreadTS: t.ts, Root: root,
		Rule: hit.Rule, Reason: hit.Reason, Target: hit.Target, CallFP: fp, Summary: summary,
		CardID: aid, CardBot: w.Bot.Name, State: store.ApprovalPending}
	if p, ok := w.primaryFor(root, hit.Rule, a.Target); ok {
		a.MergedInto, a.CardID, a.CardBot = p.AID, p.CardID, p.CardBot
		if err := w.Store.PutApproval(a); err != nil {
			w.log().Error("could not record an approval request", "err", err.Error())
			return true
		}
		w.refreshCard(p)
		w.log().Warn("dangerous action parked; merged into the open card", "rule", hit.Rule, "aid", aid, "card", p.AID)
		return true
	}
	if err := w.Store.PutApproval(a); err != nil {
		w.log().Error("could not record an approval request", "err", err.Error())
		return true
	}
	text, blocks := w.approvalCard(a, nil)
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

// primaryFor finds the open card for (rule, target) in a task tree.
func (w *Worker) primaryFor(root, rule, target string) (store.Approval, bool) {
	if root == "" {
		return store.Approval{}, false
	}
	list, _ := w.Store.ApprovalsWhere(func(a store.Approval) bool {
		return a.Root == root && a.Rule == rule && a.Target == target && a.MergedInto == "" && a.State == store.ApprovalPending
	})
	if len(list) == 0 {
		return store.Approval{}, false
	}
	sort.Slice(list, func(i, k int) bool { return list[i].Created < list[k].Created })
	return list[0], true
}

// merged lists the approvals sharing primary's card.
func (w *Worker) merged(primary string) []store.Approval {
	list, _ := w.Store.ApprovalsWhere(func(a store.Approval) bool { return a.MergedInto == primary })
	sort.Slice(list, func(i, k int) bool { return list[i].Created < list[k].Created })
	return list
}

// peer returns the worker of partner name (the card owner may be another
// partner: only its token can update its card).
func (w *Worker) peer(name string) *Worker {
	if name == "" || name == w.Bot.Name {
		return w
	}
	if p := w.Stops.worker(name); p != nil {
		return p
	}
	return w
}

func (w *Worker) refreshCard(p store.Approval) {
	owner := w.peer(p.CardBot)
	text, blocks := owner.approvalCard(p, w.merged(p.AID))
	_ = owner.Outbox.Update(context.Background(), p.CardID, text, blocks) // still pending: keep the buttons
}

func (w *Worker) approvalCard(a store.Approval, more []store.Approval) (string, json.RawMessage) {
	return w.approvalCardNote(a, more, "")
}

// approvalCardNote renders a pending card (text + three-button blocks),
// with an optional leading note (e.g. after a restart).
func (w *Worker) approvalCardNote(a store.Approval, more []store.Approval, note string) (string, json.RawMessage) {
	who := "Sin"
	if len(w.Owners) > 0 {
		who = "<@" + w.Owners[0] + ">"
	}
	target := ""
	if a.Target != "" {
		target = " on `" + strings.ReplaceAll(redact.String(a.Target), "`", "'") + "`"
	}
	text := fmt.Sprintf("%s ⚠️ *%s wants to do something that needs your approval* (`%s`%s) — 已暂挂，等 Sin 批准\n> %s\n```%s```\n",
		who, w.Bot.Name, a.Rule, target, a.Reason, strings.ReplaceAll(a.Summary, "```", "'''"))
	if note != "" {
		text = note + "\n" + text
	}
	if len(more) > 0 {
		var who []string
		for _, m := range more {
			who = append(who, m.Bot)
		}
		text += fmt.Sprintf("Also parked here (same action and target in this task): %d more call(s) from %s.\n", len(more), strings.Join(who, ", "))
	}
	text += "Only Sin's click counts. *仅此一次* lets the identical call run once; *本任务内批准* allows `" + a.Rule +
		"`" + target + " for every partner until this task is stopped or done. You can also reply `approve` / `批准`, " +
		"`approve for task` / `本任务内批准`, or `deny` / `拒绝` in this thread."
	button := func(style, id, label string) map[string]any {
		b := map[string]any{"type": "button", "action_id": id, "value": a.AID,
			"text": map[string]any{"type": "plain_text", "text": label}}
		if style != "" {
			b["style"] = style
		}
		return b
	}
	blocks := []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "actions", "elements": []any{
			button("primary", ActionApprove, "仅此一次 / Approve once"),
			button("", ActionApproveTask, "本任务内批准 / Approve for this task"),
			button("danger", ActionDeny, "拒绝 / Deny"),
		}},
	}
	b, _ := json.Marshal(blocks)
	return text, b
}

// Approve applies Sin's decision (DecideOnce, DecideTask or DecideDeny) on
// an approval card. Clicks or replies by anyone else are ignored, and so is
// a click on a card that is already decided or cancelled. It reports
// whether the decision was taken. Every partner whose call shares the card
// is told the outcome in a new turn; after an approval it may re-issue the
// identical call. DecideTask also grants (rule, target) for the task tree.
func (w *Worker) Approve(ctx context.Context, aid, user, mode string) bool {
	if !w.isOwner(user) {
		w.log().Info("approval click ignored: not Sin", "user", user)
		return false
	}
	a, ok, _ := w.Store.GetApproval(aid)
	if !ok {
		return false
	}
	if a.MergedInto != "" {
		if p, ok, _ := w.Store.GetApproval(a.MergedInto); ok {
			a = p
		}
	}
	if stopped, _ := w.Store.AnyRevoked(a.Root); stopped {
		cancelRoot(ctx, w, a.Root) // a late click on a stopped task: cancel, never run
		return false
	}
	state := store.ApprovalApproved
	if mode == DecideDeny {
		state = store.ApprovalDenied
	} else if mode != DecideTask {
		mode = DecideOnce
	}
	p, changed, err := w.Store.DecideApproval(a.AID, state, user)
	if err != nil || !changed {
		return false
	}
	group := []store.Approval{p}
	for _, m := range w.merged(p.AID) {
		if d, changed, _ := w.Store.DecideApproval(m.AID, state, user); changed {
			group = append(group, d)
		}
	}
	if state == store.ApprovalApproved {
		for _, g := range group {
			_ = w.Store.SetApprovalMode(g.AID, mode)
		}
	}
	verdict := "❌ Denied by <@" + user + ">"
	switch {
	case state == store.ApprovalApproved && mode == DecideTask:
		_ = w.Store.PutGrant(store.Grant{Root: p.Root, Rule: p.Rule, Target: p.Target, By: user, AID: p.AID})
		verdict = "✅ Approved for this task by <@" + user + "> (本任务内批准: `" + p.Rule + "` on `" + p.Target + "`)"
		group = append(group, w.grantCovered(ctx, p, user)...)
	case state == store.ApprovalApproved:
		verdict = "✅ Approved by <@" + user + "> (仅此一次 / this one call only)"
	}
	owner := w.peer(p.CardBot)
	_ = owner.Outbox.Update(ctx, p.CardID, fmt.Sprintf("%s — `%s`\n```%s```", verdict, p.Rule, p.Summary), nil)
	for _, g := range group {
		w.peer(g.Bot).tell(ctx, g, user, state == store.ApprovalApproved, mode)
	}
	return true
}

// grantCovered approves the other open cards of the tree the new grant
// covers (same rule; same target or a path below it).
func (w *Worker) grantCovered(ctx context.Context, p store.Approval, user string) []store.Approval {
	var out []store.Approval
	open, _ := w.Store.ApprovalsWhere(func(a store.Approval) bool {
		return a.Root == p.Root && a.Rule == p.Rule && a.State == store.ApprovalPending
	})
	for _, a := range open {
		if ok, _ := w.Store.GrantCovers(a.Root, a.Rule, a.Target); !ok {
			continue
		}
		if d, changed, _ := w.Store.DecideApproval(a.AID, store.ApprovalApproved, user); changed {
			_ = w.Store.SetApprovalMode(a.AID, DecideTask)
			if a.MergedInto == "" {
				_ = w.peer(a.CardBot).Outbox.Update(ctx, a.CardID, "✅ Covered by Sin's approval for this task — `"+a.Rule+"` on `"+a.Target+"`", nil)
			}
			out = append(out, d)
		}
	}
	return out
}

// tell starts a turn in the approval's thread with Sin's decision.
func (w *Worker) tell(ctx context.Context, a store.Approval, user string, ok bool, mode string) {
	note := "Sin denied the parked action (" + a.Summary + "). Do not run it; continue with other work or ask Sin."
	if ok {
		note = "Sin approved the parked action (" + a.Summary + "). Re-issue exactly the same call now; it will be allowed once."
		if mode == DecideTask {
			note = "Sin approved the parked action (" + a.Summary + ") for this whole task: `" + a.Rule + "` on " + a.Target +
				" is allowed for every partner until the task is stopped or done. Re-issue the call now."
		}
	}
	in := Inbound{Channel: a.Channel, ThreadTS: a.ThreadTS, TS: a.ThreadTS, User: user}
	t := w.thread(ctx, a.Thread, in)
	t.push(ctx, job{in: Inbound{Channel: a.Channel, ThreadTS: a.ThreadTS, TS: a.AID, User: user, Text: note},
		auth: policy.Authority{Source: policy.FromSin, Root: a.Root}})
}

// approvalReply handles Sin's text answer to the newest pending approval
// of this thread.
func (w *Worker) approvalReply(ctx context.Context, key string, in Inbound) bool {
	l := strings.ToLower(strings.Join(strings.Fields(stripMentions(in.Text)), " "))
	var mode string
	switch l {
	case "approve", "approved", "approve once", "批准", "同意", "仅此一次":
		mode = DecideOnce
	case "approve for task", "approve for this task", "approve task", "本任务内批准", "本任务批准":
		mode = DecideTask
	case "deny", "denied", "拒绝":
		mode = DecideDeny
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
	return w.Approve(ctx, newest.AID, in.User, mode)
}

// cancelRoot cancels every parked or approved-but-unused approval of a task
// tree, from the store (so it also works after a restart), updates each
// card in place and drops the tree's grants. w is any partner's worker:
// cards are updated through their owner's token.
func cancelRoot(ctx context.Context, w *Worker, root string) int {
	if root == "" {
		return 0
	}
	list, _ := w.Store.ApprovalsWhere(func(a store.Approval) bool {
		return a.Root == root && (a.State == store.ApprovalPending || a.State == store.ApprovalApproved)
	})
	n := 0
	for _, a := range list {
		if _, changed, _ := w.Store.DecideApproval(a.AID, store.ApprovalCancelled, "stop"); changed {
			n++
			if a.MergedInto == "" {
				_ = w.peer(a.CardBot).Outbox.Update(ctx, a.CardID, CancelledByStop+" — `"+a.Rule+"`\n```"+a.Summary+"```", nil)
			}
		}
	}
	if g, _ := w.Store.DeleteGrants(root); g > 0 {
		w.log().Info("grants dropped with the task", "root", root, "grants", g)
	}
	return n
}

// recoverApprovals marks cards of approvals that survived a restart.
func (w *Worker) recoverApprovals(ctx context.Context) {
	pending, _ := w.Store.Approvals(w.Bot.Name, store.ApprovalPending)
	for _, a := range pending {
		if stopped, _ := w.Store.AnyRevoked(a.Root); stopped {
			cancelRoot(ctx, w, a.Root)
			continue
		}
		if a.MergedInto != "" || (a.CardBot != "" && a.CardBot != w.Bot.Name) {
			continue
		}
		// still pending: re-send the three buttons with the note
		text, blocks := w.approvalCardNote(a, w.merged(a.AID), "⏳ *Still waiting for Sin* (Plexus restarted). Approving lets "+
			w.Bot.Name+" run it when it re-issues the call.")
		_ = w.Outbox.Update(ctx, a.CardID, text, blocks)
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
