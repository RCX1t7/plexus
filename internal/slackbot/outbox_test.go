package slackbot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/store"
)

func TestOutboxDedupAndOrigin(t *testing.T) {
	st := openStore(t)
	fp := &fakePoster{}
	ob := &Outbox{Bot: "alpha", Store: st, Poster: fp, Secrets: []string{"xoxb-live-secret-1"}}
	ids, _ := ob.Enqueue(Post{ID: "r1", Channel: "C1", Thread: "1.0", Text: "hello xoxb-live-secret-1", Kind: "reply", Origin: "stranger", HandoffID: "h1"})
	if len(ids) != 1 {
		t.Fatal(ids)
	}
	if again, _ := ob.Enqueue(Post{ID: "r1", Channel: "C1", Thread: "1.0", Text: "hello"}); len(again) != 0 {
		t.Fatal("duplicate request id enqueued")
	}
	if state, err := ob.Deliver(context.Background(), "r1"); err != nil || state != store.Sent {
		t.Fatal(state, err)
	}
	if state, _ := ob.Deliver(context.Background(), "r1"); state != store.Sent || len(fp.all()) != 1 {
		t.Fatal("redelivered")
	}
	p := fp.all()[0]
	if p.Meta.RequestID != "r1" || strings.Contains(p.Text, "xoxb-live-secret-1") {
		t.Fatalf("%+v", p)
	}
	o, ok, _ := st.GetOrigin("C1", p.TS)
	if !ok || o.Source != "stranger" || o.Handoff != "h1" || o.Bot != "alpha" {
		t.Fatalf("origin %+v", o)
	}
}

func TestOutboxUncertainReconcile(t *testing.T) {
	st := openStore(t)
	fp := &fakePoster{}
	ob := &Outbox{Bot: "alpha", Store: st, Poster: fp}
	ctx := context.Background()
	// 1. timeout after Slack actually posted: found in history -> sent, no resend
	_, _ = ob.Enqueue(Post{ID: "a", Channel: "C1", Thread: "1.0", Text: "a"})
	fp.failNext = errors.New("i/o timeout")
	if s, _ := ob.Deliver(ctx, "a"); s != store.Uncertain {
		t.Fatal(s)
	}
	fp.history = map[string]string{"a": "5.5"}
	if res, resent, _ := ob.Reconcile(ctx); res != 1 || resent != 0 {
		t.Fatal(res, resent)
	}
	if m, _ := st.Get("a"); m.State != store.Sent || m.SlackTS != "5.5" {
		t.Fatalf("%+v", m)
	}
	// 2. proven absent -> requeued and sent once
	_, _ = ob.Enqueue(Post{ID: "b", Channel: "C1", Thread: "1.0", Text: "b"})
	fp.failNext = errors.New("connection reset")
	_, _ = ob.Deliver(ctx, "b")
	if res, resent, _ := ob.Reconcile(ctx); res != 0 || resent != 1 || fp.count("b") != 1 {
		t.Fatal(res, resent, fp.count("b"))
	}
	// 3. history unreadable -> stays uncertain
	_, _ = ob.Enqueue(Post{ID: "c", Channel: "C1", Thread: "1.0", Text: "c"})
	fp.failNext = errors.New("timeout")
	_, _ = ob.Deliver(ctx, "c")
	fp.findErr = errors.New("missing_scope")
	_, _, _ = ob.Reconcile(ctx)
	if m, _ := st.Get("c"); m.State != store.Uncertain {
		t.Fatal(m.State)
	}
}

func TestOutboxCrashWhileSendingAndRateLimit(t *testing.T) {
	st := openStore(t)
	fp := &fakePoster{}
	ob := &Outbox{Bot: "alpha", Store: st, Poster: fp}
	_, _ = ob.Enqueue(Post{ID: "x", Channel: "C1", Text: "x"})
	_, _ = st.Claim("x") // the process died mid-send
	if n, _ := st.RecoverStartup("alpha"); n != 1 {
		t.Fatal(n)
	}
	if ids, _ := st.PendingIDs("alpha"); len(ids) != 0 {
		t.Fatal("crashed send must not be blindly pending")
	}
	_, _ = ob.Enqueue(Post{ID: "y", Channel: "C1", Text: "y"})
	fp.failNext = &PostError{Outcome: NotSent, RetryAfter: time.Second, Err: errors.New("ratelimited")}
	if s, _ := ob.Deliver(context.Background(), "y"); s != store.Pending {
		t.Fatal(s)
	}
	if err := ob.Flush(context.Background()); err != nil || fp.count("y") != 1 {
		t.Fatal(err, fp.count("y"))
	}
}

func TestChunks(t *testing.T) {
	long := make([]byte, 9000)
	for i := range long {
		long[i] = 'a'
	}
	if c := Chunks(string(long)); len(c) < 2 {
		t.Fatal(len(c))
	}
}
