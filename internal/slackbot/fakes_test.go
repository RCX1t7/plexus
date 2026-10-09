package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/store"
)

// fakePoster records posts; failNext makes the next post fail with err.
type fakePoster struct {
	mu       sync.Mutex
	posts    []posted
	failNext error
	history  map[string]string // request id -> ts (what Slack "has")
	findErr  error
	seq      int
}

type posted struct {
	Channel, Thread, Text string
	Meta                  Meta
	TS                    string
	Updated               bool
}

func (p *fakePoster) Post(_ context.Context, ch, th, text string, m Meta) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failNext; err != nil {
		p.failNext = nil
		return "", err
	}
	p.seq++
	ts := fmt.Sprintf("9%d.000", p.seq)
	p.posts = append(p.posts, posted{Channel: ch, Thread: th, Text: text, Meta: m, TS: ts})
	if p.history == nil {
		p.history = map[string]string{}
	}
	p.history[m.RequestID] = ts
	return ts, nil
}

func (p *fakePoster) Update(_ context.Context, ch, ts, text string, blocks json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.posts {
		if p.posts[i].TS == ts && p.posts[i].Channel == ch {
			p.posts[i].Text = text
			p.posts[i].Updated = true
			p.posts[i].Meta.Blocks = blocks // chat.update replaces the layout; nil = blocks=[]
			return nil
		}
	}
	return errors.New("message_not_found")
}

func (p *fakePoster) Find(_ context.Context, _, _, id string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.findErr != nil {
		return "", false, p.findErr
	}
	ts, ok := p.history[id]
	return ts, ok, nil
}

func (p *fakePoster) all() []posted {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]posted(nil), p.posts...)
}

func (p *fakePoster) count(sub string) int {
	n := 0
	for _, x := range p.all() {
		if strings.Contains(x.Text, sub) {
			n++
		}
	}
	return n
}

func openStore(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "plexus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeHarness is an in-process harness scripted by the last prompt line.
type fakeHarness struct {
	hostTools   bool
	noGuestLock bool
	mu          sync.Mutex
	sessions    []*fakeSess
}

func (h *fakeHarness) Name() string { return "fake" }
func (h *fakeHarness) Capabilities() harness.Capabilities {
	c := harness.Capabilities{HostTools: harness.Unsupported, GuestLock: harness.Native}
	if h.noGuestLock {
		c.GuestLock = harness.Unsupported
	}
	if h.hostTools {
		c.HostTools = harness.Native
	}
	return c
}
func (h *fakeHarness) Detect(context.Context, harness.Env) harness.DetectionResult {
	return harness.DetectionResult{}
}
func (h *fakeHarness) StartSession(_ context.Context, o harness.SessionOptions) (harness.Session, error) {
	s := &fakeSess{ev: make(chan harness.Event, 64), opts: o, interrupt: make(chan struct{}, 1), steer: make(chan string, 4)}
	h.mu.Lock()
	h.sessions = append(h.sessions, s)
	h.mu.Unlock()
	return s, nil
}

func (h *fakeHarness) turns() []harness.Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []harness.Turn
	for _, s := range h.sessions {
		out = append(out, s.sent()...)
	}
	return out
}

type fakeSess struct {
	ev         chan harness.Event
	opts       harness.SessionOptions
	interrupt  chan struct{}
	steer      chan string
	mu         sync.Mutex
	turnsSent  []harness.Turn
	steered    []string
	n          int
	running    string
	closed     bool
	interrupts int
	decisions  []harness.Decision
	results    []harness.HostResult
}

func (s *fakeSess) sent() []harness.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]harness.Turn(nil), s.turnsSent...)
}

func (s *fakeSess) ID() string                   { return "native-1" }
func (s *fakeSess) Events() <-chan harness.Event { return s.ev }
func (s *fakeSess) Control(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("no")
}
func (s *fakeSess) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ev)
	}
	return nil
}
func (s *fakeSess) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *fakeSess) emit(e harness.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.ev <- e
	}
}
func (s *fakeSess) Interrupt(context.Context) error {
	s.mu.Lock()
	s.interrupts++
	s.mu.Unlock()
	select {
	case s.interrupt <- struct{}{}:
	default:
	}
	return nil
}
func (s *fakeSess) Steer(_ context.Context, text string) error {
	s.mu.Lock()
	running := s.running != ""
	if running {
		s.steered = append(s.steered, text)
	}
	s.mu.Unlock()
	if !running {
		return errors.New("idle")
	}
	s.steer <- text
	return nil
}

func (s *fakeSess) Send(ctx context.Context, t harness.Turn) (string, error) {
	s.mu.Lock()
	s.n++
	id := fmt.Sprintf("turn-%d", s.n)
	s.turnsSent = append(s.turnsSent, t)
	s.running = id
	s.mu.Unlock()
	// the command is the last line of the frame; a stranger's text arrives
	// wrapped in <external>, so look inside the block
	body := strings.TrimSuffix(t.Text, "\n</external>")
	cmd := body[strings.LastIndex(body, "\n")+1:]
	go func() {
		final := "done: " + cmd
		f := strings.Fields(cmd)
		switch {
		case len(f) > 0 && f[0] == "SLOW":
			select {
			case <-s.interrupt:
				s.finish(id, harness.Event{Kind: harness.EventError, TurnID: id, Text: "interrupted"})
				return
			case st := <-s.steer:
				final = "steered: " + st[strings.LastIndex(st, "\n")+1:]
			case <-ctx.Done():
				s.finish(id, harness.Event{Kind: harness.EventError, TurnID: id, Text: "cancelled"})
				return
			}
		case len(f) > 1 && f[0] == "TOOL":
			got := make(chan harness.Decision, 1)
			s.emit(harness.Event{Kind: harness.EventPermission, TurnID: id,
				Perm:   &harness.PermissionRequest{Tool: harness.ToolRequest{CallID: "call-" + id, Name: f[1], Kind: harness.ToolKind(f[1]), Command: strings.Join(f[2:], " ")}},
				Decide: func(d harness.Decision) { got <- d }})
			d := <-got
			s.mu.Lock()
			s.decisions = append(s.decisions, d)
			s.mu.Unlock()
			final = fmt.Sprintf("allow=%v %s", d.Allow, d.Reason)
		case len(f) > 1 && f[0] == "TOOLJ":
			// a tool call with raw JSON input (MCP tools, per-call workdir)
			in := map[string]any{}
			_ = json.Unmarshal([]byte(strings.Join(f[2:], " ")), &in)
			got := make(chan harness.Decision, 1)
			s.emit(harness.Event{Kind: harness.EventPermission, TurnID: id,
				Perm:   &harness.PermissionRequest{Tool: harness.ToolRequest{CallID: "call-" + id, Name: f[1], Input: in}},
				Decide: func(d harness.Decision) { got <- d }})
			d := <-got
			final = fmt.Sprintf("allow=%v %s", d.Allow, d.Reason)
		case len(f) > 1 && f[0] == "HOST":
			args := map[string]any{}
			_ = json.Unmarshal([]byte(strings.Join(f[2:], " ")), &args)
			got := make(chan harness.HostResult, 1)
			s.emit(harness.Event{Kind: harness.EventHostTool, TurnID: id, Name: f[1],
				Tool: &harness.ToolRequest{CallID: "c-" + id, Name: f[1], Kind: harness.ToolMeta, Input: args},
				Call: func(r harness.HostResult) { got <- r }})
			r := <-got
			s.mu.Lock()
			s.results = append(s.results, r)
			s.mu.Unlock()
			final = "host: " + r.Text
		case len(f) > 1 && f[0] == "ASK":
			n, _ := strconv.Atoi(f[1])
			got := make(chan harness.Answers, n)
			for i := 1; i <= n; i++ {
				s.emit(harness.Event{Kind: harness.EventQuestion, ID: fmt.Sprintf("%s-q%d", id, i), TurnID: id,
					Questions: []harness.Question{{ID: "q", Text: fmt.Sprintf("question %d?", i)}},
					Answer:    func(a harness.Answers) { got <- a }})
			}
			var as []string
			for i := 0; i < n; i++ {
				a := <-got
				if a == nil {
					as = append(as, "<none>")
					continue
				}
				as = append(as, strings.Join(a["q"], ","))
			}
			final = "answered: " + strings.Join(as, " | ")
		case len(f) > 1 && f[0] == "FLOOD":
			// n harness events in one turn (tool use, extension and
			// background events, as a busy coding turn streams them)
			n, _ := strconv.Atoi(f[1])
			for i := 0; i < n; i++ {
				switch i % 4 {
				case 0, 1:
					s.emit(harness.Event{Kind: harness.EventToolUse, TurnID: id, Name: "bash"})
				case 2:
					s.emit(harness.Event{Kind: harness.EventExtension, TurnID: id, Name: "progress"})
				default:
					s.emit(harness.Event{Kind: harness.EventToolResult, TurnID: id})
				}
			}
			final = fmt.Sprintf("flooded %d", n)
		case len(f) > 0 && f[0] == "QUIET":
			final = ""
		}
		if len(f) > 0 && (f[0] == "TOOL" || f[0] == "WORK") {
			s.emit(harness.Event{Kind: harness.EventToolUse, TurnID: id}) // work evidence
		}
		s.finish(id, harness.Event{Kind: harness.EventFinal, TurnID: id, Text: final})
	}()
	return id, nil
}

func (s *fakeSess) finish(id string, e harness.Event) {
	s.mu.Lock()
	s.running = ""
	s.mu.Unlock()
	s.emit(e)
}
