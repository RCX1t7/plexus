// Package fakeslack is an in-process fake of the Slack surface Plexus uses:
// a Web API subset over net/http/httptest plus Socket Mode
// (apps.connections.open + WebSocket envelopes that must be acked).
//
// Plexus identity model (ARCHITECTURE §2, answer #3): every bot is its own
// Slack App with its own bot token (xoxb) and app-level token (xapp) and its
// own Socket Mode connection. There is no hub identity. Every message event
// is delivered to every connected app (like real Slack for apps in the
// channel); unacked envelopes are redelivered per app on reconnect.
//
// Plexus points slack-go at this server with config.json "slack_api_url"
// (= APIURL()); apps.connections.open goes through it too and returns this
// server's ws:// URL (answer #2).
//
// Duplicates are judged by the outbox request_id that Plexus writes into
// every post's metadata (event_type=plexus_msg, answer #7), never by text.
package fakeslack

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/tests/internal/wsmini"
)

// BotIdentity is one Slack App (one bot) Plexus runs.
type BotIdentity struct {
	Name     string // "claude", "codex", "dsh", "acp"
	Token    string // bot token xoxb-...
	AppToken string // app-level token xapp-... (Socket Mode)
	AppID    string // A...
	UserID   string // U... (bot user)
	BotID    string // B...
}

// Options configures the fake workspace.
type Options struct {
	TeamID     string
	Channel    string // the acceptance channel id
	OwnerID    string // Sin's user id
	Bots       []BotIdentity
	ExtraUsers []string // other humans (strangers) for negative tests
	PingEvery  time.Duration
}

// DefaultOptions returns the identities used throughout ACCEPTANCE.md. IDs
// satisfy Plexus' config validation (^[UW][A-Z0-9]{2,}$); token strings are
// deliberately short so they never match secret scanners.
func DefaultOptions() Options {
	mk := func(name, up string) BotIdentity {
		return BotIdentity{Name: name, Token: "xoxb-f-" + name, AppToken: "xapp-f-" + name, AppID: "A0" + up, UserID: "U0" + up, BotID: "B0" + up}
	}
	return Options{TeamID: "T0SIM", Channel: "C0ACCEPT", OwnerID: "U0SIN",
		Bots:       []BotIdentity{mk("claude", "CLAUDE"), mk("codex", "CODEX"), mk("dsh", "DSH"), mk("acp", "ACP")},
		ExtraUsers: []string{"U0OTHER"}, PingEvery: 5 * time.Second}
}

// Message is one message stored in the fake workspace.
type Message struct {
	TS        string         `json:"ts"`
	Channel   string         `json:"channel"`
	ThreadTS  string         `json:"thread_ts,omitempty"`
	User      string         `json:"user"`
	BotID     string         `json:"bot_id,omitempty"`
	BotName   string         `json:"bot_name,omitempty"` // fake-only: identity name
	Text      string         `json:"text"`
	Blocks    any            `json:"blocks,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	RequestID string         `json:"request_id,omitempty"` // metadata.event_payload.request_id
	Kind      string         `json:"kind,omitempty"`       // metadata.event_payload.kind
	Edited    bool           `json:"edited,omitempty"`
	At        time.Time      `json:"at"`
	ViaAPI    bool           `json:"via_api"` // true = posted by a bot via chat.postMessage
}

// Payload returns metadata.event_payload (nil if absent).
func (m Message) Payload() map[string]any {
	p, _ := m.Metadata["event_payload"].(map[string]any)
	return p
}

// Update is one chat.update call (status cards).
type Update struct {
	TS, User, Text string
	Metadata       map[string]any
	At             time.Time
}

// Reaction is one reactions.add call.
type Reaction struct {
	User, Name, Channel, TS string
	At                      time.Time
}

// Call is one Web API call observed.
type Call struct {
	Method string    `json:"method"`
	Token  string    `json:"token"`
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

type failure struct {
	status int
	err    string
}

type sock struct {
	c   *wsmini.Conn
	app string
}

// Server is the fake Slack.
type Server struct {
	opts Options
	HTTP *httptest.Server
	URL  string

	mu        sync.Mutex
	tsCounter int64
	evCounter int64
	messages  []*Message
	updates   []Update
	reactions []Reaction
	calls     []Call
	hooks     []func(Message)
	fail      map[string][]failure
	byToken   map[string]BotIdentity
	byApp     map[string]BotIdentity // app token -> identity
	sockets   map[int]sock
	sockSeq   int
	unacked   map[string]map[string]*envelope // app -> envelope_id -> envelope
	redeliv   map[string]int
	conns     map[string]int // app -> connections ever accepted
	blocked   map[string]time.Time
	clicks    []Click
	changed   chan struct{}
	stop      chan struct{}
}

type envelope struct {
	ID          string
	Payload     map[string]any
	Retry       int
	Interactive bool
}

// New starts a fake Slack server.
func New(opts Options) *Server {
	s := &Server{opts: opts, tsCounter: 1790000000000100, fail: map[string][]failure{}, byToken: map[string]BotIdentity{},
		byApp: map[string]BotIdentity{}, sockets: map[int]sock{}, unacked: map[string]map[string]*envelope{},
		redeliv: map[string]int{}, conns: map[string]int{}, blocked: map[string]time.Time{}, changed: make(chan struct{}), stop: make(chan struct{})}
	for _, b := range opts.Bots {
		s.byToken[b.Token] = b
		s.byApp[b.AppToken] = b
		s.unacked[b.Name] = map[string]*envelope{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.handleAPI)
	mux.HandleFunc("/socket", s.handleSocket)
	s.HTTP = httptest.NewServer(mux)
	s.URL = s.HTTP.URL
	if opts.PingEvery > 0 {
		go s.pinger(opts.PingEvery)
	}
	return s
}

// APIURL is config.json "slack_api_url" (slack-go OptionAPIURL style, trailing slash).
func (s *Server) APIURL() string { return s.URL + "/api/" }

// Options returns the configured identities.
func (s *Server) Options() Options { return s.opts }

// Bot returns the identity by name.
func (s *Server) Bot(name string) BotIdentity {
	for _, b := range s.opts.Bots {
		if b.Name == name {
			return b
		}
	}
	return BotIdentity{}
}

// BotByUser maps a bot user id to its name ("" for humans).
func (s *Server) BotByUser(user string) string {
	for _, b := range s.opts.Bots {
		if b.UserID == user {
			return b.Name
		}
	}
	return ""
}

// Close shuts the server and all sockets down.
func (s *Server) Close() {
	s.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	for _, k := range s.sockets {
		k.c.Close()
	}
	s.mu.Unlock()
	s.HTTP.CloseClientConnections()
	s.HTTP.Close()
}

func (s *Server) pinger(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			for _, k := range s.sockets {
				_ = k.c.Ping()
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Server) nextTSLocked() string {
	s.tsCounter++
	return fmt.Sprintf("%d.%06d", s.tsCounter/1000000, s.tsCounter%1000000)
}

// ---------------------------------------------------------------- test API

// OnPost registers a hook called synchronously for every chat.postMessage
// AFTER the message is stored and dispatched and BEFORE the HTTP response is
// written: killing the hub inside the hook reproduces "Slack accepted the
// post but Plexus never saw the response".
func (s *Server) OnPost(fn func(Message)) {
	s.mu.Lock()
	s.hooks = append(s.hooks, fn)
	s.mu.Unlock()
}

// FailNext makes the next n calls of method fail (429 adds Retry-After: 1).
func (s *Server) FailNext(method string, n int, status int, slackErr string) {
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.fail[method] = append(s.fail[method], failure{status: status, err: slackErr})
	}
	s.mu.Unlock()
}

// UserPost posts a message as Sin (the Owner). threadTS "" = top-level.
func (s *Server) UserPost(threadTS, text string) string {
	return s.UserPostAs(s.opts.OwnerID, threadTS, text)
}

// UserPostAs posts as any human user and dispatches the message event.
func (s *Server) UserPostAs(user, threadTS, text string) string {
	s.mu.Lock()
	m := &Message{TS: s.nextTSLocked(), Channel: s.opts.Channel, ThreadTS: threadTS, User: user, Text: text, At: time.Now()}
	s.messages = append(s.messages, m)
	s.dispatchLocked(m)
	s.notifyLocked()
	s.mu.Unlock()
	return m.TS
}

// Messages returns a snapshot of all messages in posting order.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.messages))
	for i, m := range s.messages {
		out[i] = *m
	}
	return out
}

// Thread returns root + replies of a thread, in order.
func (s *Server) Thread(rootTS string) []Message {
	var out []Message
	for _, m := range s.Messages() {
		if m.TS == rootTS || m.ThreadTS == rootTS {
			out = append(out, m)
		}
	}
	return out
}

// Updates returns all chat.update calls.
func (s *Server) Updates() []Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Update(nil), s.updates...)
}

// Reactions returns all reactions.add calls.
func (s *Server) Reactions() []Reaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reaction(nil), s.reactions...)
}

// Calls returns the Web API call log.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// UnknownCalls lists Web API methods the fake does not implement (each is an
// integration gap to resolve, never silently ignored).
func (s *Server) UnknownCalls() []string {
	var out []string
	for _, c := range s.Calls() {
		if c.Error == "unknown_method" {
			out = append(out, c.Method)
		}
	}
	return out
}

// AppStats reports Socket Mode bookkeeping for one app.
type AppStats struct{ Open, TotalConns, Unacked, Redelivered int }

// Stats returns per-app socket stats.
func (s *Server) Stats(app string) AppStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := AppStats{TotalConns: s.conns[app], Unacked: len(s.unacked[app]), Redelivered: s.redeliv[app]}
	for _, k := range s.sockets {
		if k.app == app {
			st.Open++
		}
	}
	return st
}

// Connected lists apps with at least one open socket.
func (s *Server) Connected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, k := range s.sockets {
		seen[k.app] = true
	}
	var out []string
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// DropApp disconnects one app's sockets (Slack-style "disconnect") and makes
// apps.connections.open fail for that app until `down` has passed, so the
// other apps can be observed working while it is offline.
func (s *Server) DropApp(app string, down time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[app] = time.Now().Add(down)
	for id, k := range s.sockets {
		if k.app == app {
			b, _ := json.Marshal(map[string]any{"type": "disconnect", "reason": "link_disabled"})
			_ = k.c.WriteText(b)
			k.c.Close()
			delete(s.sockets, id)
		}
	}
	s.notifyLocked()
}

// DropSockets disconnects every app (connection refresh).
func (s *Server) DropSockets() {
	for _, b := range s.opts.Bots {
		s.DropApp(b.Name, 0)
	}
}

// DupGroup is a set of posts sharing one outbox request_id.
type DupGroup struct {
	RequestID string
	Messages  []Message
}

// Duplicates returns bot posts that share an outbox request_id (answer #7:
// judged by the idempotency key in metadata, not by text).
func (s *Server) Duplicates() []DupGroup {
	by := map[string][]Message{}
	for _, m := range s.Messages() {
		if m.ViaAPI && m.RequestID != "" {
			by[m.RequestID] = append(by[m.RequestID], m)
		}
	}
	var out []DupGroup
	for k, ms := range by {
		if len(ms) > 1 {
			out = append(out, DupGroup{RequestID: k, Messages: ms})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

// MissingRequestID lists bot posts without metadata.event_payload.request_id.
func (s *Server) MissingRequestID() []Message {
	var out []Message
	for _, m := range s.Messages() {
		if m.ViaAPI && m.RequestID == "" {
			out = append(out, m)
		}
	}
	return out
}

// WaitFor blocks until pred(messages) is true or the timeout expires.
func (s *Server) WaitFor(timeout time.Duration, pred func([]Message) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		if pred(s.Messages()) {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		select {
		case <-ch:
		case <-time.After(min(left, 200*time.Millisecond)):
		}
	}
}

// WaitCond is WaitFor for arbitrary state (sockets, reactions, ...).
func (s *Server) WaitCond(timeout time.Duration, pred func() bool) bool {
	return s.WaitFor(timeout, func([]Message) bool { return pred() })
}

// ---------------------------------------------------------------- dispatch

func (s *Server) dispatchLocked(m *Message) {
	ev := map[string]any{"type": "message", "channel": m.Channel, "user": m.User, "text": m.Text,
		"ts": m.TS, "event_ts": m.TS, "channel_type": "channel", "team": s.opts.TeamID}
	if m.ThreadTS != "" {
		ev["thread_ts"] = m.ThreadTS
	}
	if m.BotID != "" {
		ev["bot_id"] = m.BotID // modern apps: no subtype, user = bot user
		ev["app_id"] = s.Bot(m.BotName).AppID
	}
	if m.Metadata != nil {
		ev["metadata"] = m.Metadata
	}
	for _, b := range s.opts.Bots {
		// Slack only delivers events to an app that has a live Socket Mode
		// connection; an app that never connected (e.g. a disabled bot) gets
		// nothing, so do not enqueue or track unacked for it.
		hasConn := false
		for _, k := range s.sockets {
			if k.app == b.Name {
				hasConn = true
				break
			}
		}
		if !hasConn {
			continue
		}
		s.evCounter++
		env := &envelope{ID: fmt.Sprintf("env-%s-%d", b.Name, s.evCounter)}
		env.Payload = map[string]any{"token": "unused", "team_id": s.opts.TeamID, "api_app_id": b.AppID, "type": "event_callback",
			"event_id": fmt.Sprintf("Ev%s%08d", strings.ToUpper(b.Name), s.evCounter), "event_time": m.At.Unix(), "event": ev}
		s.unacked[b.Name][env.ID] = env
		// Deliver to the first healthy connection for this app; skip past any
		// stale (e.g. post-restart) sockets whose write fails so the live one
		// still receives it (Slack delivers to the current connection).
		for id, k := range s.sockets {
			if k.app != b.Name {
				continue
			}
			if err := k.c.WriteText(envBytes(env)); err != nil {
				k.c.Close()
				delete(s.sockets, id)
				continue
			}
			break // one healthy connection per app receives it
		}
	}
}

func envBytes(e *envelope) []byte {
	reason := ""
	if e.Retry > 0 {
		reason = "timeout"
	}
	typ := "events_api"
	if e.Interactive {
		typ = "interactive"
	}
	b, _ := json.Marshal(map[string]any{"envelope_id": e.ID, "type": typ, "accepts_response_payload": e.Interactive,
		"retry_attempt": e.Retry, "retry_reason": reason, "payload": e.Payload})
	return b
}

func (s *Server) handleSocket(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	c, err := wsmini.Upgrade(w, r)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.sockSeq++
	id := s.sockSeq
	s.conns[app]++
	s.sockets[id] = sock{c: c, app: app}
	n := 0
	for _, k := range s.sockets {
		if k.app == app {
			n++
		}
	}
	hello, _ := json.Marshal(map[string]any{"type": "hello", "num_connections": n,
		"connection_info": map[string]any{"app_id": s.Bot(app).AppID}, "debug_info": map[string]any{"host": "fakeslack"}})
	_ = c.WriteText(hello)
	ids := make([]string, 0, len(s.unacked[app]))
	for k := range s.unacked[app] {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	for _, k := range ids {
		e := s.unacked[app][k]
		if s.conns[app] > 1 {
			e.Retry++
			s.redeliv[app]++
		}
		_ = c.WriteText(envBytes(e))
	}
	s.notifyLocked()
	s.mu.Unlock()
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			break
		}
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if json.Unmarshal(msg, &ack) == nil && ack.EnvelopeID != "" {
			s.mu.Lock()
			delete(s.unacked[app], ack.EnvelopeID)
			s.notifyLocked()
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	delete(s.sockets, id)
	s.notifyLocked()
	s.mu.Unlock()
}

// ---------------------------------------------------------------- Web API

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	params, err := readParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "invalid_form_data"})
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token, _ = params["token"].(string)
	}
	s.mu.Lock()
	call := Call{Method: method, Token: token, At: time.Now()}
	if fs := s.fail[method]; len(fs) > 0 {
		f := fs[0]
		s.fail[method] = fs[1:]
		call.Error = f.err
		s.calls = append(s.calls, call)
		s.mu.Unlock()
		if f.status == 429 {
			w.Header().Set("Retry-After", "1")
		}
		writeJSON(w, f.status, map[string]any{"ok": false, "error": f.err})
		return
	}
	bot, isBot := s.byToken[token]
	app, isApp := s.byApp[token]
	if method == "apps.connections.open" {
		isBot = false
	}
	if !isBot && !(isApp && method == "apps.connections.open") {
		call.Error = "invalid_auth"
		if isApp {
			call.Error = "not_allowed_token_type"
		}
		s.calls = append(s.calls, call)
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": false, "error": call.Error})
		return
	}
	var resp map[string]any
	var posted *Message
	var hooks []func(Message)
	str := func(k string) string { v, _ := params[k].(string); return v }
	switch method {
	case "auth.test":
		resp = map[string]any{"ok": true, "team_id": s.opts.TeamID, "team": "sim", "user_id": bot.UserID, "bot_id": bot.BotID,
			"user": bot.Name, "url": "https://sim.slack.com/"}
	case "apps.connections.open":
		if until, ok := s.blocked[app.Name]; ok && time.Now().Before(until) {
			resp = map[string]any{"ok": false, "error": "service_unavailable"}
			break
		}
		resp = map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(s.URL, "http") + "/socket?app=" + app.Name + "&ticket=" + fmt.Sprint(time.Now().UnixNano())}
	case "chat.postMessage":
		ch, text := str("channel"), str("text")
		if ch == "" || (text == "" && params["blocks"] == nil) {
			resp = map[string]any{"ok": false, "error": "no_text"}
			break
		}
		m := &Message{TS: s.nextTSLocked(), Channel: ch, User: bot.UserID, BotID: bot.BotID, BotName: bot.Name,
			Text: text, Blocks: params["blocks"], At: time.Now(), ViaAPI: true, ThreadTS: str("thread_ts")}
		m.Metadata = parseJSONMap(params["metadata"])
		if p := m.Payload(); p != nil {
			m.RequestID, _ = p["request_id"].(string)
			m.Kind, _ = p["kind"].(string)
		}
		s.messages = append(s.messages, m)
		s.dispatchLocked(m)
		s.notifyLocked()
		posted = m
		hooks = append(hooks, s.hooks...)
		resp = map[string]any{"ok": true, "channel": ch, "ts": m.TS, "message": msgJSON(m, true)}
	case "chat.update":
		ts := str("ts")
		resp = map[string]any{"ok": false, "error": "message_not_found"}
		for _, m := range s.messages {
			if m.TS == ts && m.User == bot.UserID {
				m.Text, m.Edited = str("text"), true
				if _, ok := params["blocks"]; ok {
					m.Blocks = params["blocks"]
				}
				if md := parseJSONMap(params["metadata"]); md != nil {
					m.Metadata = md
				}
				s.updates = append(s.updates, Update{TS: ts, User: bot.UserID, Text: m.Text, Metadata: m.Metadata, At: time.Now()})
				resp = map[string]any{"ok": true, "ts": ts, "channel": m.Channel, "text": m.Text}
				s.notifyLocked()
			}
		}
	case "conversations.replies":
		ts := str("ts")
		withMeta := str("include_all_metadata") == "true" || params["include_all_metadata"] == true
		var list []any
		for _, m := range s.messages {
			if m.TS == ts || m.ThreadTS == ts {
				list = append(list, msgJSON(m, withMeta))
			}
		}
		if list == nil {
			resp = map[string]any{"ok": false, "error": "thread_not_found"}
		} else {
			resp = map[string]any{"ok": true, "messages": list, "has_more": false, "response_metadata": map[string]any{"next_cursor": ""}}
		}
	case "conversations.history":
		withMeta := str("include_all_metadata") == "true"
		var list []any
		for i := len(s.messages) - 1; i >= 0; i-- {
			if m := s.messages[i]; m.ThreadTS == "" || m.ThreadTS == m.TS {
				list = append(list, msgJSON(m, withMeta))
			}
		}
		resp = map[string]any{"ok": true, "messages": list, "has_more": false}
	case "reactions.add":
		s.reactions = append(s.reactions, Reaction{User: bot.UserID, Name: str("name"), Channel: str("channel"), TS: str("timestamp"), At: time.Now()})
		s.notifyLocked()
		resp = map[string]any{"ok": true}
	case "users.info":
		u := str("user")
		name, isB := strings.ToLower(strings.TrimPrefix(u, "U0")), s.BotByUser(u) != ""
		resp = map[string]any{"ok": true, "user": map[string]any{"id": u, "name": name, "is_bot": isB, "real_name": name}}
	case "bots.info":
		resp = map[string]any{"ok": true, "bot": map[string]any{"id": str("bot"), "name": "bot"}}
	default:
		call.Error = "unknown_method"
		resp = map[string]any{"ok": false, "error": "unknown_method"}
	}
	if ok, _ := resp["ok"].(bool); !ok && call.Error == "" {
		call.Error, _ = resp["error"].(string)
	}
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	if posted != nil {
		for _, h := range hooks {
			h(*posted)
		}
	}
	writeJSON(w, 200, resp)
}

func msgJSON(m *Message, withMeta bool) map[string]any {
	out := map[string]any{"type": "message", "ts": m.TS, "user": m.User, "text": m.Text}
	if m.ThreadTS != "" {
		out["thread_ts"] = m.ThreadTS
	}
	if m.BotID != "" {
		out["bot_id"] = m.BotID
	}
	if withMeta && m.Metadata != nil {
		out["metadata"] = m.Metadata
	}
	return out
}

func parseJSONMap(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			return m
		}
	}
	return nil
}

func readParams(r *http.Request) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range r.URL.Query() {
		out[k] = v[0]
	}
	if r.Body == nil {
		return out, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return out, nil
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		for k, v := range m {
			out[k] = v
		}
		return out, nil
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	for k, v := range vals {
		out[k] = v[0]
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Button is one interactive button found in a message's blocks.
type Button struct{ ActionID, Value, Text, BlockID string }

// Buttons extracts Block Kit buttons (actions blocks) from a message.
func (m Message) Buttons() []Button {
	var out []Button
	blocks, _ := m.Blocks.([]any)
	if s, ok := m.Blocks.(string); ok {
		_ = json.Unmarshal([]byte(s), &blocks)
	}
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm["type"] != "actions" {
			continue
		}
		bid, _ := bm["block_id"].(string)
		els, _ := bm["elements"].([]any)
		for _, e := range els {
			em, _ := e.(map[string]any)
			if em["type"] != "button" {
				continue
			}
			btn := Button{BlockID: bid}
			btn.ActionID, _ = em["action_id"].(string)
			btn.Value, _ = em["value"].(string)
			if t, ok := em["text"].(map[string]any); ok {
				btn.Text, _ = t["text"].(string)
			}
			out = append(out, btn)
		}
	}
	return out
}

// Click delivers a block_actions interaction (a user pressing a button on
// msgTS) to the app that posted the message, over its Socket Mode
// connection ("interactive" envelope), with redelivery like events.
func (s *Server) Click(user, msgTS string, b Button) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msg *Message
	for _, m := range s.messages {
		if m.TS == msgTS {
			msg = m
		}
	}
	if msg == nil || msg.BotName == "" {
		return fmt.Errorf("no bot message %s", msgTS)
	}
	app := s.Bot(msg.BotName)
	s.evCounter++
	now := fmt.Sprintf("%d.000%d", time.Now().Unix(), s.evCounter%1000)
	payload := map[string]any{"type": "block_actions", "team": map[string]any{"id": s.opts.TeamID}, "api_app_id": app.AppID,
		"user":    map[string]any{"id": user, "username": strings.ToLower(user), "team_id": s.opts.TeamID},
		"channel": map[string]any{"id": msg.Channel}, "trigger_id": fmt.Sprintf("trig-%d", s.evCounter),
		"container": map[string]any{"type": "message", "message_ts": msg.TS, "channel_id": msg.Channel, "is_ephemeral": false},
		"message":   map[string]any{"type": "message", "ts": msg.TS, "thread_ts": msg.ThreadTS, "text": msg.Text, "user": msg.User, "metadata": msg.Metadata},
		"actions": []any{map[string]any{"type": "button", "action_id": b.ActionID, "block_id": b.BlockID, "value": b.Value,
			"text": map[string]any{"type": "plain_text", "text": b.Text}, "action_ts": now}}}
	env := &envelope{ID: fmt.Sprintf("env-int-%s-%d", app.Name, s.evCounter), Payload: payload, Interactive: true}
	s.unacked[app.Name][env.ID] = env
	s.clicks = append(s.clicks, Click{User: user, TS: msgTS, Value: b.Value, At: time.Now()})
	for id, k := range s.sockets {
		if k.app == app.Name {
			if err := k.c.WriteText(envBytes(env)); err != nil {
				k.c.Close()
				delete(s.sockets, id)
			}
			break
		}
	}
	s.notifyLocked()
	return nil
}

// Click is a recorded button press.
type Click struct {
	User, TS, Value string
	At              time.Time
}

// Clicks returns all recorded button presses.
func (s *Server) Clicks() []Click {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Click(nil), s.clicks...)
}

// SocketStats aggregates per-app Socket Mode stats across all apps:
// (open connections, total connections ever, unacked envelopes, redelivered).
func (s *Server) SocketStats() (open, total, unacked, redelivered int) {
	for _, b := range s.Options().Bots {
		st := s.Stats(b.Name)
		open += st.Open
		total += st.TotalConns
		unacked += st.Unacked
		redelivered += st.Redelivered
	}
	return
}

// PendingApproval finds the newest APPROVAL card in rootTS's thread.
func (s *Server) PendingApproval(rootTS string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found Message
	ok := false
	for _, m := range s.messages {
		if (m.ThreadTS == rootTS || m.TS == rootTS) && m.Kind == "APPROVAL" {
			found, ok = *m, true
		}
	}
	return found, ok
}

// UnackedDump returns "app|type|text" for every still-unacked envelope (debug).
func (s *Server) UnackedDump() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for app, m := range s.unacked {
		for _, e := range m {
			typ, txt := "?", ""
			if ev, ok := e.Payload["event"].(map[string]any); ok {
				typ, _ = ev["type"].(string)
				txt, _ = ev["text"].(string)
				if u, _ := ev["user"].(string); u != "" {
					typ = typ + ":" + u
				}
			}
			if len(txt) > 30 {
				txt = txt[:30]
			}
			out = append(out, app+"|"+typ+"|"+txt)
		}
	}
	sort.Strings(out)
	return out
}
