package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "plexus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOutboxStates(t *testing.T) {
	s := open(t)
	m := Msg{RequestID: "r1", Bot: "claude", Channel: "C1", ThreadTS: "1.0", Text: "hi", Kind: "reply"}
	if fresh, _ := s.Enqueue(m); !fresh {
		t.Fatal("first enqueue not fresh")
	}
	if fresh, _ := s.Enqueue(m); fresh {
		t.Fatal("duplicate request id accepted")
	}
	if won, _ := s.Claim("r1"); !won {
		t.Fatal("claim failed")
	}
	if won, _ := s.Claim("r1"); won {
		t.Fatal("double claim")
	}
	// crash while sending -> uncertain, never pending
	if n, _ := s.RecoverStartup("claude"); n != 1 {
		t.Fatalf("recover = %d", n)
	}
	if ids, _ := s.UncertainIDs("claude"); len(ids) != 1 {
		t.Fatal("not uncertain")
	}
	if ok, _ := s.Requeue("r1"); !ok {
		t.Fatal("requeue")
	}
	if ids, _ := s.PendingIDs("claude"); len(ids) != 1 {
		t.Fatal("not pending")
	}
	_, _ = s.Claim("r1")
	if err := s.Finish("r1", Sent, "111.222", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("r1")
	if got.State != Sent || got.SlackTS != "111.222" {
		t.Fatalf("%+v", got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plexus.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.MarkSeen("claude", "C1:1.0")
	_ = s.Revoke("C1:1.0", "sin")
	_ = s.SaveSession("claude", "C1:1.0", Session{NativeID: "n1", RootTask: "C1:1.0", Channel: "C1", ThreadTS: "1.0"})
	_ = s.UpdateSession("claude", "C1:1.0", func(v *Session) { v.Inflight, v.InflightSource = "1.5", "sin" })
	s.Close()
	if _, err := Open(p); err != nil { // reopen
		t.Fatal(err)
	}
}

func TestSeenRevokedSessionsOriginsHandoffsQuestions(t *testing.T) {
	s := open(t)
	if f, _ := s.MarkSeen("claude", "C1:1.0"); !f {
		t.Fatal("fresh")
	}
	if f, _ := s.MarkSeen("claude", "C1:1.0"); f {
		t.Fatal("dup")
	}
	if f, _ := s.MarkSeen("codex", "C1:1.0"); !f {
		t.Fatal("per partner")
	}
	_ = s.Revoke("C1:1.0", "sin")
	if r, _ := s.AnyRevoked("x", "C1:1.0"); !r {
		t.Fatal("revoked")
	}
	if l, _ := s.Revocations(5); len(l) != 1 {
		t.Fatal(l)
	}
	_ = s.SaveSession("claude", "C1:1.0", Session{NativeID: "n1", RootTask: "C1:1.0", Channel: "C1", ThreadTS: "1.0"})
	_ = s.SaveSession("claude", "C1:1.0", Session{RootTask: "C1:2.0"}) // merges
	v, ok, _ := s.LoadSession("claude", "C1:1.0")
	if !ok || v.NativeID != "n1" || v.RootTask != "C1:2.0" {
		t.Fatalf("%+v", v)
	}
	_ = s.UpdateSession("claude", "C1:1.0", func(v *Session) { v.Inflight = "1.5" })
	if in, _ := s.Inflight("claude"); len(in) != 1 {
		t.Fatal("inflight")
	}
	_ = s.PutOrigin("C1", "1.6", Origin{Bot: "claude", Source: "stranger", Handoff: "h1"})
	if o, ok, _ := s.GetOrigin("C1", "1.6"); !ok || o.Source != "stranger" || o.Handoff != "h1" {
		t.Fatalf("%+v", o)
	}
	rec, _ := json.Marshal(map[string]string{"task": "t", "done_when": "d"})
	_ = s.PutHandoff(Handoff{TaskID: "h1", ParentTask: "C1:1.0", FromBot: "claude", ToBot: "codex", Record: rec})
	if h, ok, _ := s.GetHandoff("h1"); !ok || h.ToBot != "codex" {
		t.Fatalf("%+v", h)
	}
	_ = s.PutQuestion(Question{Thread: "C1:1.0", Bot: "claude", QID: "q1", State: QuestionOpen})
	if q, _ := s.OpenQuestions("claude"); len(q) != 1 {
		t.Fatal("open q")
	}
	_ = s.SetQuestionState("C1:1.0", "claude", "q1", QuestionLost)
	if q, _ := s.OpenQuestions("claude"); len(q) != 0 {
		t.Fatal("q still open")
	}
	s.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	_ = s.Prune(24 * time.Hour)
	if f, _ := s.MarkSeen("claude", "C1:1.0"); !f {
		t.Fatal("prune kept old dedup record")
	}
}
