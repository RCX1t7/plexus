package fakeslack

import (
	"testing"
	"time"

	"github.com/RCX1t7/plexus/tests/internal/slackc"
)

func TestPostRecordAndRequestIDDuplicates(t *testing.T) {
	s := New(DefaultOptions())
	defer s.Close()
	c := slackc.New(s.APIURL())
	if _, err := c.Call("auth.test", "xoxb-f-claude", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call("auth.test", "xoxb-bogus", nil); err == nil {
		t.Fatal("bogus token must be rejected")
	}
	root := s.UserPost("", "goal")
	meta := map[string]any{"event_type": "plexus_msg", "event_payload": map[string]any{"request_id": "r-1", "kind": "result"}}
	for i := 0; i < 2; i++ { // same request_id twice = duplicate
		if _, err := c.Call("chat.postMessage", "xoxb-f-claude", map[string]any{"channel": "C0ACCEPT", "thread_ts": root, "text": "plan", "metadata": meta}); err != nil {
			t.Fatal(err)
		}
	}
	// same text, different request_id = NOT a duplicate (text may legitimately repeat)
	meta2 := map[string]any{"event_type": "plexus_msg", "event_payload": map[string]any{"request_id": "r-2", "kind": "result"}}
	if _, err := c.Call("chat.postMessage", "xoxb-f-claude", map[string]any{"channel": "C0ACCEPT", "thread_ts": root, "text": "plan", "metadata": meta2}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call("chat.postMessage", "xoxb-f-dsh", map[string]any{"channel": "C0ACCEPT", "thread_ts": root, "text": "no meta"}); err != nil {
		t.Fatal(err)
	}
	th := s.Thread(root)
	if len(th) != 5 || th[1].RequestID != "r-1" || th[1].Kind != "result" || th[1].User != "U0CLAUDE" {
		t.Fatalf("thread = %+v", th)
	}
	if d := s.Duplicates(); len(d) != 1 || d[0].RequestID != "r-1" {
		t.Fatalf("dups %+v", d)
	}
	if m := s.MissingRequestID(); len(m) != 1 || m[0].Text != "no meta" {
		t.Fatalf("missing %+v", m)
	}
	out, err := c.Call("conversations.replies", "xoxb-f-codex", map[string]any{"channel": "C0ACCEPT", "ts": root, "include_all_metadata": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["messages"].([]any)[1].(map[string]any)["metadata"]; !ok {
		t.Fatal("metadata must be returned with include_all_metadata")
	}
	if _, err := c.Call("reactions.add", "xoxb-f-codex", map[string]any{"channel": "C0ACCEPT", "timestamp": root, "name": "eyes"}); err != nil {
		t.Fatal(err)
	}
	if r := s.Reactions(); len(r) != 1 || r[0].User != "U0CODEX" || r[0].Name != "eyes" {
		t.Fatalf("reactions %+v", r)
	}
	if _, err := c.Call("chat.scheduleMessage", "xoxb-f-claude", nil); err == nil || len(s.UnknownCalls()) != 1 {
		t.Fatal("unknown methods must fail loudly and be recorded")
	}
}

func TestPerAppSocketsDropAndRedelivery(t *testing.T) {
	s := New(DefaultOptions())
	defer s.Close()
	c := slackc.New(s.APIURL())
	if _, err := c.OpenSocket("xoxb-f-claude"); err == nil {
		t.Fatal("bot token must not open a socket")
	}
	socks := map[string]*slackc.Socket{}
	for _, app := range []string{"claude", "codex"} {
		sk, err := c.OpenSocket("xapp-f-" + app)
		if err != nil {
			t.Fatal(err)
		}
		if e, _ := sk.Next(); e.Type != "hello" {
			t.Fatalf("first frame %q", e.Type)
		}
		socks[app] = sk
	}
	ts := s.UserPost("", "hello")
	for app, sk := range socks { // every app gets its own envelope
		e, err := sk.Next()
		if err != nil || e.Event()["ts"] != ts {
			t.Fatalf("%s envelope %+v %v", app, e, err)
		}
		if app == "claude" {
			_ = sk.Ack(e.EnvelopeID)
		}
	}
	s.DropApp("codex", 500*time.Millisecond)
	if !s.WaitCond(2*time.Second, func() bool { return s.Stats("codex").Open == 0 }) || s.Stats("claude").Open != 1 {
		t.Fatalf("drop must only affect codex: %+v %+v", s.Stats("codex"), s.Stats("claude"))
	}
	if _, err := c.OpenSocket("xapp-f-codex"); err == nil {
		t.Fatal("codex must stay offline during the drop window")
	}
	time.Sleep(600 * time.Millisecond)
	sk, err := c.OpenSocket("xapp-f-codex")
	if err != nil {
		t.Fatal(err)
	}
	defer sk.Close()
	sk.Next()
	e, err := sk.Next()
	if err != nil || e.RetryAttempt != 1 || e.Event()["ts"] != ts {
		t.Fatalf("codex redelivery %+v %v", e, err)
	}
	if st := s.Stats("claude"); st.Redelivered != 0 || st.Unacked != 0 {
		t.Fatalf("claude acked; nothing to redeliver: %+v", st)
	}
}

func TestHookRunsBeforeResponseAndFaults(t *testing.T) {
	s := New(DefaultOptions())
	defer s.Close()
	c := slackc.New(s.APIURL())
	var hooked []string
	s.OnPost(func(m Message) { hooked = append(hooked, m.Text) })
	s.FailNext("chat.postMessage", 1, 429, "ratelimited")
	if _, err := c.Call("chat.postMessage", "xoxb-f-dsh", map[string]any{"channel": "C0ACCEPT", "text": "x"}); err != nil {
		t.Fatalf("client should retry 429: %v", err)
	}
	if len(hooked) != 1 || hooked[0] != "x" {
		t.Fatalf("hook = %v", hooked)
	}
	if calls := s.Calls(); len(calls) != 2 || calls[0].Error != "ratelimited" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestButtonsAndClickOverSocket(t *testing.T) {
	s := New(DefaultOptions())
	defer s.Close()
	c := slackc.New(s.APIURL())
	sk, err := c.OpenSocket("xapp-f-codex")
	if err != nil {
		t.Fatal(err)
	}
	defer sk.Close()
	sk.Next() // hello
	root := s.UserPost("", "goal")
	sk.Next() // the root message event
	blocks := `[{"type":"section","text":{"type":"mrkdwn","text":"approve?"}},{"type":"actions","block_id":"appr-1","elements":[` +
		`{"type":"button","action_id":"plexus_approve","value":"r-9","text":{"type":"plain_text","text":"Approve"}},` +
		`{"type":"button","action_id":"plexus_deny","value":"r-9","text":{"type":"plain_text","text":"Deny"}}]}]`
	out, err := c.Call("chat.postMessage", "xoxb-f-codex", map[string]any{"channel": "C0ACCEPT", "thread_ts": root, "text": "approval", "blocks": blocks})
	if err != nil {
		t.Fatal(err)
	}
	sk.Next() // own post event
	ts := out["ts"].(string)
	var msg Message
	for _, m := range s.Messages() {
		if m.TS == ts {
			msg = m
		}
	}
	btns := msg.Buttons()
	if len(btns) != 2 || btns[0].ActionID != "plexus_approve" || btns[1].Text != "Deny" {
		t.Fatalf("buttons %+v", btns)
	}
	if err := s.Click("U0SIN", ts, btns[0]); err != nil {
		t.Fatal(err)
	}
	e, err := sk.Next()
	if err != nil || e.Type != "interactive" {
		t.Fatalf("interactive envelope %+v %v", e, err)
	}
	if e.Payload["type"] != "block_actions" || e.Payload["user"].(map[string]any)["id"] != "U0SIN" {
		t.Fatalf("payload %+v", e.Payload)
	}
	if len(s.Clicks()) != 1 {
		t.Fatal("click not recorded")
	}
}
