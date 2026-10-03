package refhub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/tests/internal/hclient"
)

// ------------------------------------------------------------------ workers

type item struct {
	frame string
	guest bool
}

type worker struct {
	h                  *Hub
	bot, channel, root string
	mu                 sync.Mutex
	queue              []item
	running            bool
	sess               *hclient.Session
	guest              bool
	turns              int
}

var plexusTools = []string{"plexus_post", "plexus_ack", "plexus_delegate", "plexus_progress", "plexus_deliver", "plexus_review", "plexus_status", "plexus_stop_tree"}

func (h *Hub) worker(bot, channel, root string) *worker {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := bot + "|" + root
	w := h.workers[k]
	if w == nil {
		w = &worker{h: h, bot: bot, channel: channel, root: root}
		h.workers[k] = w
	}
	return w
}

func (w *worker) push(frame string, guest bool) {
	w.mu.Lock()
	w.queue = append(w.queue, item{frame, guest})
	if !w.running {
		w.running = true
		go w.loop()
	}
	w.mu.Unlock()
}

func (w *worker) loop() {
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		g := w.queue[0].guest
		var frames []string
		n := 0
		for n < len(w.queue) && w.queue[n].guest == g {
			frames = append(frames, w.queue[n].frame)
			n++
		}
		w.queue = w.queue[n:]
		w.mu.Unlock()
		w.turn(strings.Join(frames, "\n\n"), g)
	}
}

func protoOf(harness string) string {
	switch harness {
	case "claude_code", "claude":
		return hclient.Claude
	case "codex":
		return hclient.Codex
	case "dsh":
		return hclient.DSH
	}
	return hclient.ACP
}

func (w *worker) session() (*hclient.Session, error) {
	w.mu.Lock()
	s := w.sess
	w.mu.Unlock()
	if s != nil && !s.Exited(0) {
		return s, nil
	}
	h := w.h
	b := h.bots[w.bot]
	key := w.bot + "|" + w.root
	h.mu.Lock()
	resume := h.st.Sessions[key]
	h.mu.Unlock()
	if h.brk("no_resume") {
		resume = "" // mutation: start a fresh session -> completed tools re-run
	}
	proto := protoOf(b.Harness)
	tools := plexusTools
	if proto == hclient.ACP {
		tools = nil
	}
	s, err := hclient.Start(proto, hclient.Options{Exe: b.Exe, Args: b.Args, Dir: b.Workdir, ResumeID: resume, HostTools: tools,
		Approval: "untrusted", Setpgid: true}, hclient.Handler{
		Permission: func(t hclient.Tool) bool { return h.permission(w, t) },
		Question:   func(q hclient.Question) (string, bool) { return h.question(w, q) },
		HostTool:   func(name string, args map[string]any) bool { return h.hostTool(w, name, args) },
	})
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.sess = s
	w.mu.Unlock()
	h.noteSession(key, s)
	return s, nil
}

func (h *Hub) noteSession(key string, s *hclient.Session) {
	if p := s.Pid(); p > 0 {
		h.mu.Lock()
		h.st.Pids[key] = p
		h.save()
		h.mu.Unlock()
	}
	if id := s.ID(); id != "" {
		h.mu.Lock()
		h.st.Sessions[key] = id
		h.save()
		h.mu.Unlock()
	}
}

func (h *Hub) stopped(bot, root string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.st.Tasks[root]
	return t == nil || t.Stopped || t.StoppedBots[bot]
}

func (w *worker) turn(prompt string, guest bool) {
	h := w.h
	key := w.bot + "|" + w.root
	if h.stopped(w.bot, w.root) {
		return
	}
	w.mu.Lock()
	w.turns++
	n := w.turns
	w.guest = guest
	w.mu.Unlock()
	if h.brk("turn_cap") && n > 20 {
		logf("turn cap reached for %s", key)
		return
	}
	h.mu.Lock()
	h.st.Inflight[key] = prompt
	h.save()
	h.mu.Unlock()
	s, err := w.session()
	if err != nil {
		logf("start %s: %v", w.bot, err)
		return
	}
	level := "full"
	if guest {
		_ = s.SetMode("plan")
		level = "chat"
	}
	var capT *time.Timer
	if h.brk("turn_cap") {
		capT = time.AfterFunc(8*time.Second, func() { _ = s.Interrupt() })
	}
	final, err := s.Prompt(prompt, level)
	if capT != nil {
		capT.Stop()
	}
	if guest {
		_ = s.SetMode("default")
	}
	h.noteSession(key, s)
	if err != nil && strings.Contains(err.Error(), "exited") {
		return // hub restart / stop: keep Inflight for resume unless stopped
	}
	h.mu.Lock()
	delete(h.st.Inflight, key)
	h.save()
	h.mu.Unlock()
	if final == "" || h.stopped(w.bot, w.root) {
		return
	}
	meta := map[string]any{"kind": "MSG", "author": h.bots[w.bot].UserID}
	if guest {
		meta["src"] = "guest"
	}
	h.post(w.bot, w.channel, w.root, final, meta, hash(w.bot, w.root, "final", prompt, final), nil)
}

// halt: native interrupt (+stop_task) and stdin EOF now, Job/process-group
// kill after the 3 s grace.
func (w *worker) halt() {
	w.mu.Lock()
	w.queue = nil
	s := w.sess
	w.mu.Unlock()
	if s == nil {
		return
	}
	pid := s.Pid()
	go s.Interrupt()
	time.Sleep(150 * time.Millisecond)
	s.CloseStdin()
	if !s.Exited(3 * time.Second) {
		if w.h.brk("no_kill") {
			return
		}
		s.Kill()
	}
	// Final Job/process-group sweep: even on a graceful exit a child may have
	// orphaned a grandchild; reap the whole group (harmless if already gone).
	if !w.h.brk("no_kill") {
		killPGID(pid)
	}
}

// resume re-runs turns that were in flight when the previous hub died, and
// re-prompts pending approvals on their ORIGINAL card.
func (h *Hub) resume() {
	h.mu.Lock()
	var pend []*Approval
	for _, a := range h.st.Approvals {
		if a.State == "parked" {
			pend = append(pend, a)
		}
	}
	infl := map[string]string{}
	for k, v := range h.st.Inflight {
		infl[k] = v
	}
	h.mu.Unlock()
	for _, a := range pend {
		if h.brk("no_card_update") {
			ts := h.post(a.Bot, a.Channel, a.Thread, h.cardText(a, "仍待批准"), map[string]any{"kind": "APPROVAL", "approval_id": a.ID, "rule": a.Rule},
				randHex(8), h.cardBlocks(a))
			h.mu.Lock()
			a.CardTS = ts
			h.save()
			h.mu.Unlock()
			continue
		}
		h.updateCard(a, "仍待批准（Plexus 已重启）", true)
	}
	for k, prompt := range infl {
		bot, root, _ := strings.Cut(k, "|")
		h.mu.Lock()
		t := h.st.Tasks[root]
		h.mu.Unlock()
		if t == nil || h.bots[bot] == nil || h.stopped(bot, root) {
			continue
		}
		h.worker(bot, t.Channel, root).push("[plexus] 上一轮被中断；先核对实际状态，已完成的不要重做。\n"+prompt, false)
	}
}

// ------------------------------------------------------------------ permission + danger gate

func (h *Hub) permission(w *worker, t hclient.Tool) bool {
	if h.stopped(w.bot, w.root) {
		return false
	}
	w.mu.Lock()
	guest := w.guest
	w.mu.Unlock()
	if guest {
		return false
	}
	rule, target, summary := h.classify(w.bot, t.Command, t.Kind)
	if rule == "" {
		return true // workdir reads/writes, tests, builds, normal commit/push, web reads
	}
	return h.park(w, rule, target, summary, "exec", "", "")
}

func (h *Hub) granted(root, rule, target string) bool {
	t := h.st.Tasks[root]
	return t != nil && t.Grants[rule+"\x00"+target]
}

// park implements the non-blocking gate. It returns true only when the action
// is already cleared (a 本任务内 grant, or a 仅此一次 approval being consumed
// now); otherwise it parks the action (one card, three buttons) and returns
// false immediately so the harness can keep doing independent work.
func (h *Hub) park(w *worker, rule, target, summary, pkind, postCh, postText string) bool {
	fp := hash(w.root, rule, target)
	h.mu.Lock()
	if h.granted(w.root, rule, target) {
		h.mu.Unlock()
		return true
	}
	var a *Approval
	for _, x := range h.st.Approvals {
		if x.FP == fp && x.Thread == w.root {
			a = x
		}
	}
	if a != nil {
		switch a.State {
		case "approved_once":
			a.State = "consumed"
			h.save()
			h.mu.Unlock()
			return true
		case "approved_task":
			h.grant(w.root, rule, target)
			h.save()
			h.mu.Unlock()
			return true
		default: // parked | denied | stopped | consumed
			h.mu.Unlock()
			return false
		}
	}
	h.st.Seq++
	a = &Approval{ID: fmt.Sprintf("ap%d", h.st.Seq), Channel: w.channel, Thread: w.root, Bot: w.bot, Rule: rule,
		Target: target, FP: fp, Summary: summary, PKind: pkind, PostChannel: postCh, PostText: postText,
		RequestID: hash("approval", fp), State: "parked", Created: time.Now()}
	h.st.Approvals[a.ID] = a
	h.save()
	h.mu.Unlock()
	ts := h.post(w.bot, w.channel, w.root, h.cardText(a, "waiting on you"),
		map[string]any{"kind": "APPROVAL", "approval_id": a.ID, "rule": rule, "target": target}, a.RequestID, h.cardBlocks(a))
	h.mu.Lock()
	a.CardTS = ts
	h.save()
	h.mu.Unlock()
	h.armTimeout(a)
	return false
}

func (h *Hub) armTimeout(a *Approval) {
	if !h.brk("gate_timeout") {
		return
	}
	time.AfterFunc(5*time.Second, func() {
		h.mu.Lock()
		if a.State == "parked" {
			a.State = "approved_once" // mutation: a timeout auto-approves
		}
		h.save()
		h.mu.Unlock()
	})
}

func (h *Hub) grant(root, rule, target string) {
	t := h.st.Tasks[root]
	if t == nil {
		return
	}
	if t.Grants == nil {
		t.Grants = map[string]bool{}
	}
	t.Grants[rule+"\x00"+target] = true
}

func (h *Hub) cardText(a *Approval, status string) string {
	what := a.Summary
	return fmt.Sprintf("<@%s> %s — 危险操作待批（%s）：%s 想执行 `%s`（规则 %s，目标 %s）",
		h.cfg.Owner, status, status, a.Bot, what, a.Rule, a.Target)
}

func (h *Hub) cardBlocks(a *Approval) []any {
	return []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": h.cardText(a, "waiting on you")}},
		map[string]any{"type": "actions", "block_id": "plexus_approval:" + a.ID, "elements": []any{
			map[string]any{"type": "button", "action_id": "plexus_once", "value": a.ID, "style": "primary", "text": map[string]any{"type": "plain_text", "text": "仅此一次"}},
			map[string]any{"type": "button", "action_id": "plexus_task", "value": a.ID, "text": map[string]any{"type": "plain_text", "text": "本任务内批准"}},
			map[string]any{"type": "button", "action_id": "plexus_deny", "value": a.ID, "style": "danger", "text": map[string]any{"type": "plain_text", "text": "拒绝"}},
		}},
	}
}

// updateCard rewrites the card text in place (chat.update, same ts), dropping
// the buttons once the approval is decided/cancelled.
func (h *Hub) updateCard(a *Approval, status string, buttons bool) {
	params := map[string]any{"channel": a.Channel, "ts": a.CardTS, "text": h.cardText(a, status),
		"metadata": map[string]any{"event_type": "plexus_msg", "event_payload": map[string]any{"kind": "APPROVAL", "approval_id": a.ID, "request_id": a.RequestID, "state": a.State}}}
	if buttons {
		params["blocks"] = h.cardBlocks(a)
	} else {
		params["blocks"] = []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": h.cardText(a, status)}}}
	}
	_, _ = h.slack.Call("chat.update", h.tokens[a.Bot][0], params)
}

func (h *Hub) onClick(p map[string]any) {
	user, _ := p["user"].(map[string]any)
	uid, _ := user["id"].(string)
	acts, _ := p["actions"].([]any)
	if len(acts) == 0 {
		return
	}
	act, _ := acts[0].(map[string]any)
	aid, _ := act["action_id"].(string)
	id, _ := act["value"].(string)
	h.mu.Lock()
	a := h.st.Approvals[id]
	if a == nil || a.State != "parked" {
		h.mu.Unlock()
		return
	}
	if uid != h.cfg.Owner && !h.brk("any_click") {
		h.mu.Unlock()
		logf("ignored approval click by %s on %s", uid, id)
		return
	}
	status := "已拒绝"
	switch aid {
	case "plexus_once":
		a.State, status = "approved_once", "已批准（仅此一次）"
	case "plexus_task":
		a.State, status = "approved_task", "已批准（本任务内）"
		h.grant(a.Thread, a.Rule, a.Target)
	default:
		a.State = "denied"
	}
	post := a.PKind == "post" && (aid == "plexus_once" || aid == "plexus_task")
	h.save()
	h.mu.Unlock()
	if post {
		// (g) a parked outbound message is sent by Plexus itself, exactly once.
		h.post(a.Bot, a.PostChannel, "", a.PostText, map[string]any{"kind": "MSG", "author": h.bots[a.Bot].UserID},
			hash(a.Bot, a.PostChannel, "ext", a.PostText), nil)
		h.mu.Lock()
		if a.State == "approved_once" {
			a.State = "consumed"
		}
		h.save()
		h.mu.Unlock()
	}
	h.updateCard(a, status, false)
}

// ------------------------------------------------------------------ classify (reference)

var splitRE = regexp.MustCompile(`&&|\|\||[;|\n]`)
var wrapRE = regexp.MustCompile(`^(?:sh|bash|cmd|powershell|pwsh)(?:\.exe)?\s+(?:-c|/c|-Command)\s+"([^"]*)"\s*$`)

func (h *Hub) classify(bot, cmd, kind string) (rule, target, summary string) {
	if h.brk("no_gate") || cmd == "" {
		return "", "", ""
	}
	d := h.cfg.Dangerous
	defaults := d.UseDefaults == nil || *d.UseDefaults
	for _, seg := range splitRE.Split(cmd, -1) {
		seg = strings.TrimSpace(seg)
		if m := wrapRE.FindStringSubmatch(seg); m != nil {
			seg = m[1]
		}
		for _, re := range d.ExtraCommands {
			if r, err := regexp.Compile(re); err == nil && r.MatchString(seg) {
				return "extra_commands", firstArgTarget(seg), cmd
			}
		}
		if !defaults {
			continue
		}
		if ru := h.defaultRule(bot, seg); ru != "" && !h.disabled(ru) {
			return ru, firstArgTarget(seg), cmd
		}
	}
	return "", "", ""
}

// firstArgTarget is a coarse target for grant scoping (本任务内批准 is per
// class AND target): the first non-flag argument, else the executable.
func firstArgTarget(seg string) string {
	f := strings.Fields(seg)
	for len(f) > 0 && strings.Contains(f[0], "=") {
		f = f[1:]
	}
	if len(f) == 0 {
		return ""
	}
	exe := strings.TrimSuffix(strings.ToLower(filepath.Base(f[0])), ".exe")
	for _, a := range f[1:] {
		if !strings.HasPrefix(a, "-") {
			return exe + ":" + a
		}
	}
	return exe
}

func (h *Hub) disabled(rule string) bool {
	for _, x := range h.cfg.Dangerous.Disable {
		if x == rule || (strings.HasSuffix(x, ".*") && strings.HasPrefix(rule, strings.TrimSuffix(x, "*"))) || strings.HasPrefix(rule, x+".") {
			return true
		}
	}
	return false
}

func (h *Hub) defaultRule(bot, seg string) string {
	f := strings.Fields(seg)
	for len(f) > 0 && strings.Contains(f[0], "=") {
		f = f[1:]
	}
	if len(f) == 0 {
		return ""
	}
	exe := strings.TrimSuffix(strings.ToLower(filepath.Base(f[0])), ".exe")
	args := f[1:]
	low := strings.ToLower(seg)
	switch exe {
	case "git":
		if len(args) == 0 {
			return ""
		}
		switch args[0] {
		case "push":
			for _, a := range args[1:] {
				if a == "-f" || strings.HasPrefix(a, "--force") || a == "--mirror" || a == "--delete" || a == "-d" ||
					strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":") {
					return "git.force"
				}
			}
		case "filter-branch", "filter-repo":
			return "git.rewrite"
		}
	case "bfg":
		return "git.rewrite"
	case "rm", "del", "erase", "rd", "rmdir", "remove-item", "ri", "rimraf":
		wd := h.bots[bot].Workdir
		for _, a := range args {
			if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "/") && len(a) <= 3 && !strings.Contains(a[1:], "/") {
				continue
			}
			if strings.ContainsAny(a, "~$%") {
				return "fs.delete_outside"
			}
			p := a
			if !filepath.IsAbs(p) {
				p = filepath.Join(wd, p)
			}
			p = filepath.Clean(p)
			if rel, err := filepath.Rel(wd, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "fs.delete_outside"
			}
		}
	case "reg":
		if len(args) > 0 && regexp.MustCompile(`(?i)^(add|delete|import|copy|restore|load)$`).MatchString(args[0]) {
			return "sys.registry"
		}
	case "regedit":
		return "sys.registry"
	case "sc":
		if len(args) > 0 && regexp.MustCompile(`(?i)^(create|config|delete|stop|start)$`).MatchString(args[0]) {
			return "sys.service"
		}
	case "schtasks":
		if regexp.MustCompile(`(?i)/(create|delete|change)`).MatchString(seg) {
			return "sys.schtask"
		}
	case "setx":
		if regexp.MustCompile(`(?i)\s/m\b`).MatchString(seg) {
			return "sys.env_machine"
		}
	case "msiexec", "choco", "winget":
		return "sys.installer"
	case "send-mailmessage":
		return "out.mail"
	}
	if regexp.MustCompile(`(?i)(new|set|remove)-itemproperty|(new|remove|set)-item\s+.*(hkcu:|hklm:|registry::)`).MatchString(low) {
		return "sys.registry"
	}
	if regexp.MustCompile(`hooks\.slack\.com|discord\.com/api/webhooks`).MatchString(low) {
		return "out.webhook"
	}
	return ""
}

// ------------------------------------------------------------------ questions

func (h *Hub) question(w *worker, q hclient.Question) (string, bool) {
	ch := make(chan string, 1)
	h.mu.Lock()
	h.qs[w.root] = ch
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.qs, w.root); h.mu.Unlock() }()
	txt := fmt.Sprintf("<@%s> %s", h.cfg.Owner, q.Text)
	if len(q.Options) > 0 {
		txt += "（选项：" + strings.Join(q.Options, " / ") + "）"
	}
	h.post(w.bot, w.channel, w.root, txt, map[string]any{"kind": "QUESTION"}, hash(w.bot, w.root, "q", q.Text), nil)
	for {
		select {
		case a := <-ch:
			return a, true
		case <-time.After(200 * time.Millisecond):
			if h.stopped(w.bot, w.root) {
				return "", false
			}
		}
	}
}

// ------------------------------------------------------------------ host tools

func strs(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = x
	case string:
		out = []string{x}
	}
	return out
}

func sval(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (h *Hub) hostTool(w *worker, name string, args map[string]any) bool {
	h.mu.Lock()
	t := h.st.Tasks[w.root]
	h.mu.Unlock()
	if t == nil || h.stopped(w.bot, w.root) {
		return false
	}
	w.mu.Lock()
	guest := w.guest
	w.mu.Unlock()
	me := h.bots[w.bot].UserID
	switch name {
	case "plexus_post":
		text := sval(args, "text")
		if ch := sval(args, "channel"); ch != "" && ch != t.Channel && ch != h.cfg.Owner && h.byUser[ch] == "" {
			allowed := false
			for _, r := range h.cfg.Dangerous.AllowedRecipients {
				allowed = allowed || r == ch
			}
			if guest {
				return false
			}
			if !allowed && !h.brk("no_gate") && !h.disabled("out.post") {
				// non-blocking: park; Plexus itself sends it after approval (g).
				return h.park(w, "out.post", "channel:"+ch, "plexus_post → "+ch, "post", ch, text)
			}
			h.post(w.bot, ch, "", text, map[string]any{"kind": "MSG", "author": me}, hash(w.bot, ch, "ext", text), nil)
			return true
		}
		meta := map[string]any{"kind": "MSG", "author": me}
		if guest {
			meta["src"] = "guest"
		}
		h.post(w.bot, t.Channel, w.root, text, meta, hash(w.bot, w.root, "post", text), nil)
		return true
	case "plexus_progress", "plexus_status":
		return !guest
	}
	if guest {
		return false
	}
	switch name {
	case "plexus_ack":
		node := sval(args, "node")
		h.mu.Lock()
		var dw []string
		id := node
		if node == "root" {
			if t.RootDoneWhen == nil {
				t.RootDoneWhen = strs(args["done_when"])
			}
			dw = t.RootDoneWhen
		} else if d := h.delegTo(t, w.bot); d != nil {
			if d.State == "open" {
				d.State = "acked"
			}
			dw, id = d.DoneWhen, d.ID
		} else {
			h.mu.Unlock()
			return false
		}
		h.save()
		h.mu.Unlock()
		h.post(w.bot, t.Channel, w.root, fmt.Sprintf("ACK %s · done_when：%s", id, strings.Join(dw, "；")),
			map[string]any{"kind": "ACK", "node": id, "done_when": dw, "author": me}, hash(w.bot, w.root, "ack", id), nil)
		return true
	case "plexus_delegate":
		to := h.byUser[sval(args, "to")]
		if to == "" || h.bots[to] == nil {
			return false
		}
		h.mu.Lock()
		var d *Deleg
		for _, x := range t.Delegs {
			if x.From == w.bot && x.To == to && x.Task == sval(args, "task") && x.State != "stopped" {
				d = x
			}
		}
		if d == nil {
			d = &Deleg{ID: fmt.Sprintf("H%d", len(t.Delegs)+1), From: w.bot, To: to, Task: sval(args, "task"), Inputs: strs(args["inputs"]),
				DoneWhen: strs(args["done_when"]), Evidence: strs(args["evidence"]), Tried: strs(args["tried_failed"]),
				OwnerIfStuck: sval(args, "owner_if_stuck"), State: "open"}
			t.Delegs = append(t.Delegs, d)
		}
		has := false
		for _, p := range t.Participants {
			has = has || p == to
		}
		if !has {
			t.Participants = append(t.Participants, to)
		}
		h.save()
		h.mu.Unlock()
		text := sval(args, "text")
		if !strings.Contains(text, "<@"+h.bots[to].UserID+">") {
			text = "<@" + h.bots[to].UserID + "> " + text
		}
		meta := map[string]any{"kind": "HANDOFF", "node": d.ID, "from": me, "to": h.bots[to].UserID}
		for _, k := range []string{"task", "inputs", "done_when", "evidence", "tried_failed", "owner_if_stuck"} {
			if v, ok := args[k]; ok {
				meta[k] = v
			}
		}
		h.post(w.bot, t.Channel, w.root, text, meta, hash(w.bot, w.root, "delegate", d.ID), nil)
		return true
	case "plexus_deliver":
		node := sval(args, "node")
		h.mu.Lock()
		var d *Deleg
		dw := t.RootDoneWhen
		id := "root"
		if node != "root" {
			if d = h.delegTo(t, w.bot); d == nil {
				h.mu.Unlock()
				return false
			}
			dw, id = d.DoneWhen, d.ID
		}
		h.mu.Unlock()
		var lines []string
		for _, a := range args["artifacts"].([]any) {
			am, _ := a.(map[string]any)
			p := sval(am, "path")
			data, err := os.ReadFile(filepath.Join(h.bots[w.bot].Workdir, p))
			sum := sha256.Sum256(data)
			if err != nil || hex.EncodeToString(sum[:]) != sval(am, "sha256") {
				logf("deliver %s: artifact %s missing or sha mismatch: returned to %s", id, p, w.bot)
				return false
			}
			lines = append(lines, sval(am, "sha256")+"  "+p)
		}
		if v, ok := args["done_when"]; ok && h.brk("mutable_done_when") {
			dw = strs(v)
		}
		h.mu.Lock()
		if d != nil {
			d.State = "delivered"
		}
		h.save()
		h.mu.Unlock()
		text := fmt.Sprintf("%s\n```\n%s\n```\n证据：%s", sval(args, "summary"), strings.Join(lines, "\n"), strings.Join(strs(args["evidence"]), "；"))
		h.post(w.bot, t.Channel, w.root, text, map[string]any{"kind": "RESULT", "node": id, "author": me, "done_when": dw,
			"evidence": strs(args["evidence"]), "artifacts": lines}, hash(w.bot, w.root, "deliver", id), nil)
		return true
	case "plexus_review":
		h.mu.Lock()
		var d *Deleg
		for _, x := range t.Delegs {
			if x.From == w.bot && x.State == "delivered" && d == nil {
				d = x
			}
		}
		if d == nil {
			h.mu.Unlock()
			return false
		}
		kind := "ACCEPT"
		if sval(args, "decision") == "reopen" {
			kind = "REOPEN"
			d.State = "acked"
			d.Tried = append(d.Tried, sval(args, "reason"))
		} else {
			d.State = "accepted"
		}
		h.save()
		h.mu.Unlock()
		h.post(w.bot, t.Channel, w.root, fmt.Sprintf("%s %s：%s", kind, d.ID, sval(args, "reason")),
			map[string]any{"kind": kind, "node": d.ID, "reviewer": me, "author": h.bots[d.To].UserID, "done_when": d.DoneWhen},
			hash(w.bot, w.root, "review", d.ID), nil)
		return true
	case "plexus_stop_tree":
		return h.stopSub(w.root, h.byUser[sval(args, "to")], w.bot)
	}
	return false
}

func (h *Hub) delegTo(t *Task, bot string) *Deleg {
	var d *Deleg
	for _, x := range t.Delegs {
		if x.To == bot && x.State != "stopped" {
			d = x
		}
	}
	return d
}

// ------------------------------------------------------------------ stop

func (h *Hub) cancelApprovals(root, bot string) []*Approval {
	var out []*Approval
	if h.brk("no_stop_cancel") {
		return nil
	}
	for _, a := range h.st.Approvals {
		if a.Thread == root && (bot == "" || a.Bot == bot) &&
			(a.State == "parked" || a.State == "approved_once" || a.State == "approved_task") {
			a.State = "stopped"
			out = append(out, a)
		}
	}
	return out
}

func (h *Hub) stopTree(root string) {
	h.mu.Lock()
	t := h.st.Tasks[root]
	if t == nil || t.Stopped {
		h.mu.Unlock()
		return
	}
	t.Stopped = true
	for _, d := range t.Delegs {
		d.State = "stopped"
	}
	for k := range h.st.Inflight {
		if strings.HasSuffix(k, "|"+root) {
			delete(h.st.Inflight, k)
		}
	}
	cancelled := h.cancelApprovals(root, "")
	h.st.Seq++
	seq := h.st.Seq
	h.save()
	var ws []*worker
	for k, w := range h.workers {
		if strings.HasSuffix(k, "|"+root) {
			ws = append(ws, w)
		}
	}
	lead := t.Lead
	h.mu.Unlock()
	for _, a := range cancelled {
		go h.updateCard(a, "已随 stop 取消", false)
	}
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func(w *worker) { defer wg.Done(); w.halt() }(w)
	}
	wg.Wait()
	h.post(lead, t.Channel, root, "已停止", map[string]any{"kind": "STOP", "scope": "tree"}, hash(root, "stopped", fmt.Sprint(seq)), nil)
}

func (h *Hub) stopSub(root, target, by string) bool {
	h.mu.Lock()
	t := h.st.Tasks[root]
	var d *Deleg
	if t != nil {
		for _, x := range t.Delegs {
			if x.From == by && x.To == target && x.State != "stopped" && x.State != "accepted" {
				d = x
			}
		}
	}
	if d == nil {
		h.mu.Unlock()
		return false
	}
	d.State = "stopped"
	t.StoppedBots[target] = true
	delete(h.st.Inflight, target+"|"+root)
	cancelled := h.cancelApprovals(root, target)
	h.save()
	w := h.workers[target+"|"+root]
	h.mu.Unlock()
	go func() {
		for _, a := range cancelled {
			h.updateCard(a, "已随 stop 取消", false)
		}
		if w != nil {
			w.halt()
		}
		h.post(by, t.Channel, root, "已停止（子任务 "+d.ID+"）", map[string]any{"kind": "STOP", "scope": "subtree", "node": d.ID},
			hash(root, "substop", d.ID), nil)
	}()
	return true
}
