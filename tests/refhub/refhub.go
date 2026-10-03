// Package refhub is the REFERENCE STUB hub: a small, stdlib-only stand-in
// for Plexus that implements the behaviour the acceptance suite checks
// (REQUIREMENTS.md + ARCHITECTURE.md + ANSWERS-ACCEPTANCE-5.md), so the
// suite can prove its own checks pass on a correct hub and fail on a broken
// one (REFHUB_BREAK=<flag,...>, mutation tests). It is NOT Plexus: state is
// a JSON file instead of bbolt, Linux process groups instead of Job Objects.
//
// Breaks: no_dedupe no_kill no_guard no_gate any_click gate_timeout
// no_card_update no_stop_cancel turn_cap mutable_done_when no_eyes
// anyone_stops ignore_ungated no_overrides no_wrap
package refhub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/tests/internal/slackc"
)

// ------------------------------------------------------------------ config

type BotCfg struct {
	Name          string   `json:"name"`
	Harness       string   `json:"harness"`
	Enabled       bool     `json:"enabled"`
	Workdir       string   `json:"workdir"`
	UserID        string   `json:"user_id"`
	Exe           string   `json:"exe"`
	Args          []string `json:"args"`
	AppID         string   `json:"app_id"`
	StrangerGuard *bool    `json:"stranger_guard"`
	UngatedOK     bool     `json:"ungated_ok"`
}

type DangerCfg struct {
	UseDefaults       *bool    `json:"use_defaults"`
	Disable           []string `json:"disable"`
	ExtraCommands     []string `json:"extra_commands"`
	ExtraMCPTools     []string `json:"extra_mcp_tools"`
	AllowedRecipients []string `json:"allowed_recipients"`
}

type Config struct {
	Owner         string           `json:"owner"`
	StrangerGuard *bool            `json:"stranger_guard"`
	Bots          []BotCfg         `json:"bots"`
	SetupPort     int              `json:"setup_port"`
	SlackAPIURL   string           `json:"slack_api_url"`
	Dangerous     DangerCfg        `json:"dangerous_actions"`
	ACP           []map[string]any `json:"acp_harnesses"`
}

func home() string {
	if h := os.Getenv("PLEXUS_HOME"); h != "" {
		return h
	}
	return "."
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile(filepath.Join(home(), "config.json"))
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, json.Unmarshal(b, &c)
}

// ------------------------------------------------------------------ state

type OutItem struct {
	Status, TS, Channel, Thread string
}

type Deleg struct {
	ID, From, To, Task string // From/To: bot names
	Inputs, DoneWhen   []string
	Evidence, Tried    []string
	OwnerIfStuck       string
	State              string // open | acked | delivered | accepted | stopped
}

type Task struct {
	Channel, Root, Lead string
	Participants        []string
	Stopped             bool
	StoppedBots         map[string]bool
	RootDoneWhen        []string
	Delegs              []*Deleg
	Grants              map[string]bool // "rule\x00target" -> 本任务内批准 (whole tree)
}

// Approval is a parked dangerous action awaiting Sin. The gate is
// NON-BLOCKING: the harness is denied immediately with 已暂挂 and keeps doing
// independent work; Sin's click later lets the re-issued call through
// (仅此一次) or grants the class+target for the whole task tree (本任务内批准).
type Approval struct {
	ID, Channel, Thread, Bot, Rule, Target, FP, Summary, CardTS, RequestID, State string
	PKind, PostChannel, PostText                                                  string // PKind: exec | post
	Created                                                                       time.Time
}

type State struct {
	Seen      map[string]bool
	Outbox    map[string]*OutItem
	Sessions  map[string]string // bot|thread -> native id
	Inflight  map[string]string // bot|thread -> prompt
	Tasks     map[string]*Task  // root ts -> task
	Approvals map[string]*Approval
	Seq       int
	Pids      map[string]int // bot|thread -> harness pid (prev generation cleaned up on restart)
}

// ------------------------------------------------------------------ hub

type Hub struct {
	cfg    *Config
	breaks map[string]bool
	slack  *slackc.Client
	tokens map[string][2]string // bot -> bot token, app token

	mu      sync.Mutex
	st      State
	path    string
	workers map[string]*worker     // bot|thread
	qs      map[string]chan string // thread -> pending question answer
	byUser  map[string]string      // user id -> bot name
	bots    map[string]*BotCfg
}

func (h *Hub) brk(s string) bool { return h.breaks[s] }

func (h *Hub) save() {
	b, _ := json.Marshal(&h.st)
	tmp := h.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	_, _ = f.Write(b)
	_ = f.Sync()
	_ = f.Close()
	_ = os.Rename(tmp, h.path)
}

func hash(parts ...string) string {
	s := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(s[:8])
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, "refhub: "+format+"\n", a...) }

// Main is the refhub entry point: `refhub run|stop <task-id>|--version|detect`.
func Main(args []string) int {
	if len(args) == 0 {
		args = []string{"run"}
	}
	switch args[0] {
	case "--version", "version":
		fmt.Println("refhub 0.0.0 (reference stub, not Plexus)")
		return 0
	case "detect":
		fmt.Println("refhub: detect is not implemented by the reference stub")
		return 0
	case "stop":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: stop <channel:ts>")
			return 2
		}
		return cliStop(args[1])
	case "run":
		return run()
	}
	fmt.Fprintln(os.Stderr, "unknown command", args[0])
	return 2
}

func cliStop(task string) int {
	var rj struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	b, err := os.ReadFile(filepath.Join(home(), "run.json"))
	if err == nil {
		err = json.Unmarshal(b, &rj)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plexus is not running:", err)
		return 1
	}
	body, _ := json.Marshal(map[string]string{"task": task})
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/ipc/stop", rj.Port), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rj.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "stop failed:", resp.Status)
		return 1
	}
	fmt.Println("stopped", task)
	return 0
}

var forbiddenArgs = regexp.MustCompile(`^(--dangerously-skip-permissions|bypassPermissions|--permission-mode=bypassPermissions|--full-auto|danger-full-access|--yolo|--dangerously-bypass-approvals-and-sandbox)$`)

func run() int {
	cfg, err := loadConfig()
	if err != nil {
		logf("config: %v", err)
		return 1
	}
	h := &Hub{cfg: cfg, breaks: map[string]bool{}, slack: slackc.New(cfg.SlackAPIURL), tokens: map[string][2]string{},
		path: filepath.Join(home(), "refhub-state.json"), workers: map[string]*worker{},
		qs: map[string]chan string{}, byUser: map[string]string{}, bots: map[string]*BotCfg{}}
	for _, b := range strings.Split(os.Getenv("REFHUB_BREAK"), ",") {
		if b = strings.TrimSpace(b); b != "" {
			h.breaks[b] = true
		}
	}
	h.st = State{Seen: map[string]bool{}, Outbox: map[string]*OutItem{}, Sessions: map[string]string{}, Inflight: map[string]string{},
		Tasks: map[string]*Task{}, Approvals: map[string]*Approval{}, Pids: map[string]int{}}
	if b, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(b, &h.st)
	}
	if h.st.Pids == nil {
		h.st.Pids = map[string]int{}
	}
	for k, pid := range h.st.Pids { // a previous generation's harnesses (Job Object would have reaped them)
		killPGID(pid)
		delete(h.st.Pids, k)
	}
	h.save()
	var started []string
	for i := range cfg.Bots {
		b := &cfg.Bots[i]
		if !b.Enabled {
			continue
		}
		up := strings.ToUpper(b.Name)
		h.tokens[b.Name] = [2]string{os.Getenv("PLEXUS_BOT_TOKEN_" + up), os.Getenv("PLEXUS_APP_TOKEN_" + up)}
		h.byUser[b.UserID] = b.Name
		ungated := ""
		for _, a := range b.Args {
			if forbiddenArgs.MatchString(a) {
				ungated = a
			}
		}
		if ungated != "" && !b.UngatedOK && !h.brk("ignore_ungated") {
			logf("partner %s not started: %s leaves no blocking permission callback (set bots[].ungated_ok to accept the risk)", b.Name, ungated)
			continue
		}
		h.bots[b.Name] = b
		started = append(started, b.Name)
	}
	if err := h.serveIPC(); err != nil {
		logf("ipc: %v", err)
		return 1
	}
	h.reconcile()
	ready := make(chan string, len(started))
	for _, name := range started {
		go h.socketLoop(name, ready)
	}
	for range started {
		<-ready
	}
	fmt.Printf("plexus ready bots=%d\n", len(started))
	h.resume()
	select {}
}

// ------------------------------------------------------------------ IPC

func (h *Hub) serveIPC() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", h.cfg.SetupPort))
	if err != nil {
		return err
	}
	token := randHex(32)
	rj, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "port": ln.Addr().(*net.TCPAddr).Port, "token": token})
	if err := os.WriteFile(filepath.Join(home(), "run.json"), rj, 0o600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ipc/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		var req struct{ Task string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, ts, _ := strings.Cut(req.Task, ":")
		h.mu.Lock()
		t := h.st.Tasks[ts]
		h.mu.Unlock()
		if t == nil {
			http.Error(w, "no such task", 404)
			return
		}
		go h.stopTree(ts)
		w.Write([]byte("ok"))
	})
	go http.Serve(ln, mux)
	return nil
}

// ------------------------------------------------------------------ Slack I/O

func (h *Hub) post(bot, channel, thread, text string, meta map[string]any, rid string, blocks any) string {
	if h.brk("no_dedupe") {
		rid = randHex(8)
	}
	h.mu.Lock()
	if it := h.st.Outbox[rid]; it != nil && it.Status == "sent" {
		h.mu.Unlock()
		return it.TS
	}
	h.st.Outbox[rid] = &OutItem{Status: "sending", Channel: channel, Thread: thread}
	h.save()
	h.mu.Unlock()
	if meta == nil {
		meta = map[string]any{}
	}
	meta["request_id"] = rid
	params := map[string]any{"channel": channel, "text": text,
		"metadata": map[string]any{"event_type": "plexus_msg", "event_payload": meta}}
	if thread != "" {
		params["thread_ts"] = thread
	}
	if blocks != nil {
		bj, _ := json.Marshal(blocks)
		params["blocks"] = string(bj)
	}
	var ts string
	for i := 0; i < 5; i++ {
		out, err := h.slack.Call("chat.postMessage", h.tokens[bot][0], params)
		if err == nil {
			ts, _ = out["ts"].(string)
			break
		}
		time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
	}
	h.mu.Lock()
	h.st.Outbox[rid] = &OutItem{Status: "sent", TS: ts, Channel: channel, Thread: thread}
	h.save()
	h.mu.Unlock()
	return ts
}

// reconcile: outbox items left "sending" by a crash are looked up by
// request_id in conversations.replies(include_all_metadata).
func (h *Hub) reconcile() {
	h.mu.Lock()
	var todo []string
	for rid, it := range h.st.Outbox {
		if it.Status == "sending" {
			todo = append(todo, rid)
		}
	}
	h.mu.Unlock()
	for _, rid := range todo {
		h.mu.Lock()
		it := h.st.Outbox[rid]
		h.mu.Unlock()
		tok := ""
		for _, t := range h.tokens {
			tok = t[0]
			break
		}
		out, err := h.slack.Call("conversations.replies", tok, map[string]any{"channel": it.Channel, "ts": it.Thread, "include_all_metadata": true})
		found := ""
		if err == nil {
			msgs, _ := out["messages"].([]any)
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				md, _ := mm["metadata"].(map[string]any)
				ep, _ := md["event_payload"].(map[string]any)
				if ep["request_id"] == rid {
					found, _ = mm["ts"].(string)
				}
			}
		}
		h.mu.Lock()
		if found != "" {
			it.Status, it.TS = "sent", found
		} else {
			delete(h.st.Outbox, rid) // re-posted when the resumed turn produces it again
		}
		h.save()
		h.mu.Unlock()
	}
}

func (h *Hub) socketLoop(bot string, ready chan<- string) {
	first := true
	for {
		sk, err := h.slack.OpenSocket(h.tokens[bot][1])
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		for {
			env, err := sk.Next()
			if err != nil {
				break
			}
			switch env.Type {
			case "hello":
				if first {
					first = false
					ready <- bot
				}
			case "disconnect":
				_ = sk.Close()
			case "events_api":
				h.onEvent(bot, sk, env)
			case "interactive":
				_ = sk.Ack(env.EnvelopeID)
				h.onClick(env.Payload)
			}
		}
		_ = sk.Close()
		time.Sleep(200 * time.Millisecond)
	}
}

var mentionRE = regexp.MustCompile(`<@([A-Z0-9]+)>`)

func (h *Hub) role(user string) string {
	if user == h.cfg.Owner {
		return "owner"
	}
	if b := h.byUser[user]; b != "" {
		return "partner"
	}
	return "stranger"
}

func (h *Hub) guardOn(bot string) bool {
	if h.brk("no_guard") {
		return false
	}
	if b := h.bots[bot]; b != nil && b.StrangerGuard != nil && !h.brk("no_overrides") {
		return *b.StrangerGuard
	}
	return h.cfg.StrangerGuard == nil || *h.cfg.StrangerGuard
}

func (h *Hub) onEvent(bot string, sk *slackc.Socket, env slackc.Envelope) {
	ev := env.Event()
	key := bot + "|" + env.EventID()
	h.mu.Lock()
	dup := h.st.Seen[key]
	h.st.Seen[key] = true
	h.save()
	h.mu.Unlock()
	_ = sk.Ack(env.EnvelopeID)
	if dup || ev["type"] != "message" {
		return
	}
	user, _ := ev["user"].(string)
	text, _ := ev["text"].(string)
	ts, _ := ev["ts"].(string)
	thread, _ := ev["thread_ts"].(string)
	channel, _ := ev["channel"].(string)
	me := h.bots[bot]
	if user == me.UserID || user == "" {
		return
	}
	src := ""
	if md, ok := ev["metadata"].(map[string]any); ok {
		if ep, ok := md["event_payload"].(map[string]any); ok {
			src, _ = ep["src"].(string)
		}
	}
	role := h.role(user)
	if role == "partner" && src == "guest" {
		role = "stranger"
	}
	var mentioned []string
	for _, m := range mentionRE.FindAllStringSubmatch(text, -1) {
		if b := h.byUser[m[1]]; b != "" {
			mentioned = append(mentioned, b)
		}
	}
	isMentioned := false
	for _, m := range mentioned {
		isMentioned = isMentioned || m == bot
	}
	root := thread
	if root == "" {
		root = ts
	}

	h.mu.Lock()
	t := h.st.Tasks[root]
	if thread == "" && t == nil && len(mentioned) > 0 {
		t = &Task{Channel: channel, Root: root, Lead: mentioned[0], StoppedBots: map[string]bool{}}
		h.st.Tasks[root] = t
		h.save()
	}
	if t == nil {
		h.mu.Unlock()
		return
	}
	participant := false
	for _, p := range t.Participants {
		participant = participant || p == bot
	}
	trimmed := strings.TrimSpace(text)
	isStop := thread != "" && (trimmed == "stop" || trimmed == "停")
	if isStop && (role == "owner" || h.brk("anyone_stops")) {
		lead := t.Lead
		h.mu.Unlock()
		if bot == lead {
			go h.stopTree(root)
		}
		return
	}
	if q := h.qs[root]; q != nil && role == "owner" && thread != "" {
		h.mu.Unlock()
		if bot == t.Lead || isMentioned {
			select {
			case q <- text:
			default:
			}
		}
		return
	}
	if !isMentioned && !participant {
		h.mu.Unlock()
		return
	}
	if role == "owner" && t.Stopped && !isStop {
		t.Stopped = false // Sin speaking again in a stopped thread starts a new root
		t.StoppedBots = map[string]bool{}
	}
	if t.Stopped || t.StoppedBots[bot] {
		h.mu.Unlock()
		return
	}
	guest := role == "stranger" && h.guardOn(bot)
	if guest && (me.Harness == "codex" || me.Harness == "fake_acp" || strings.HasPrefix(me.Harness, "acp")) {
		h.mu.Unlock()
		return // Codex/ACP: no hard deny-all mode -> do not take stranger messages
	}
	if !participant && !guest {
		t.Participants = append(t.Participants, bot)
		h.save()
	}
	if thread == "" && bot == t.Lead && !h.brk("no_eyes") {
		go h.slack.Call("reactions.add", h.tokens[bot][0], map[string]any{"channel": channel, "timestamp": ts, "name": "eyes"})
	}
	h.mu.Unlock()
	frame := h.frame(user, role, channel, root, text)
	h.worker(bot, channel, root).push(frame, guest)
}

var quoteLine = regexp.MustCompile(`^(>|&gt;)`)

func (h *Hub) frame(user, role, channel, root, text string) string {
	hdr := fmt.Sprintf("[Slack message from <@%s>, thread %s:%s]", user, channel, root)
	if role == "stranger" {
		hdr = fmt.Sprintf("[Slack message from <@%s> (stranger), thread %s:%s]", user, channel, root)
		if !h.brk("no_wrap") {
			text = fmt.Sprintf("<external src=\"slack:%s\">\n%s\n</external>", user, text)
		}
		return hdr + "\n" + text
	}
	if h.brk("no_wrap") {
		return hdr + "\n" + text
	}
	var out []string
	inQ := false
	for _, l := range strings.Split(text, "\n") {
		q := quoteLine.MatchString(l)
		if q && !inQ {
			out = append(out, `<external src="quote">`)
		}
		if !q && inQ {
			out = append(out, "</external>")
		}
		inQ = q
		out = append(out, l)
	}
	if inQ {
		out = append(out, "</external>")
	}
	return hdr + "\n" + strings.Join(out, "\n")
}
