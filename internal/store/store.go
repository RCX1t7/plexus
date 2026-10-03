// Package store is the single bbolt file (pure Go key/value store) shared
// by every bot of this hub: outbox, inbound dedup, task revocations,
// session bindings, message origins, handoff records and questions.
//
// bbolt locks the file for one process. A second process (the `plexus
// stop` CLI) therefore talks to the running hub over its local control
// endpoint and only opens the file itself when no hub is running.
package store

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Outbox states. "uncertain" means a send may or may not have reached
// Slack; it is reconciled against Slack history, never blindly resent.
const (
	Pending   = "pending"
	Sending   = "sending"
	Sent      = "sent"
	Failed    = "failed"
	Uncertain = "uncertain"
)

var (
	bOutbox    = []byte("outbox")
	bSeen      = []byte("seen")
	bRevoked   = []byte("revoked")
	bSessions  = []byte("sessions")
	bOrigins   = []byte("origins")
	bHandoffs  = []byte("handoffs")
	bQuestions = []byte("questions")
)

// ErrNotFound is returned for missing records.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct {
	db  *bolt.DB
	now func() time.Time
}

// Open opens (and creates) the database at path. It fails after a second
// if another process holds the file.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bOutbox, bSeen, bRevoked, bSessions, bOrigins, bHandoffs, bQuestions} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ts() int64 { return s.now().UnixNano() }

func get[T any](tx *bolt.Tx, bucket []byte, key string) (T, bool) {
	var v T
	raw := tx.Bucket(bucket).Get([]byte(key))
	if raw == nil {
		return v, false
	}
	return v, json.Unmarshal(raw, &v) == nil
}

func put(tx *bolt.Tx, bucket []byte, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), b)
}

// Msg is one outbound Slack message.
type Msg struct {
	RequestID, Bot, Channel, ThreadTS, Text string
	Kind                                    string // reply, post, handoff, deliver, stop_ack, ... (Slack metadata)
	Origin                                  string // trust source of the turn that wrote it ("stranger" taints)
	HandoffID                               string // handoff record carried by this message, if any
	State, SlackTS, Error                   string
	Created                                 int64
}

// Enqueue stores a message unless its request id exists. The boolean
// reports whether it was new (false = duplicate, ignored).
func (s *Store) Enqueue(m Msg) (bool, error) {
	fresh := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		if _, ok := get[Msg](tx, bOutbox, m.RequestID); ok {
			return nil
		}
		fresh = true
		m.State, m.Created = Pending, s.ts()
		return put(tx, bOutbox, m.RequestID, m)
	})
	return fresh, err
}

// transition moves id from state `from` to `to` (from "" = any), applying
// edit. It reports whether the move happened.
func (s *Store) transition(id, from, to string, edit func(*Msg)) (bool, error) {
	moved := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		m, ok := get[Msg](tx, bOutbox, id)
		if !ok || (from != "" && m.State != from) {
			return nil
		}
		m.State = to
		if edit != nil {
			edit(&m)
		}
		moved = true
		return put(tx, bOutbox, id, m)
	})
	return moved, err
}

// Claim moves a pending message to sending. Only one caller can win.
func (s *Store) Claim(id string) (bool, error) { return s.transition(id, Pending, Sending, nil) }

// Release returns a claimed message to pending (definitely not sent).
func (s *Store) Release(id, reason string) error {
	_, err := s.transition(id, Sending, Pending, func(m *Msg) { m.Error = reason })
	return err
}

// Finish records the outcome of a claimed (or uncertain) message.
func (s *Store) Finish(id, state, slackTS, errMsg string) error {
	_, err := s.transition(id, "", state, func(m *Msg) { m.SlackTS, m.Error = slackTS, errMsg })
	return err
}

// Requeue moves an uncertain message back to pending after reconciliation
// proved it never reached Slack.
func (s *Store) Requeue(id string) (bool, error) { return s.transition(id, Uncertain, Pending, nil) }

// Get loads one message.
func (s *Store) Get(id string) (Msg, error) {
	var m Msg
	var ok bool
	_ = s.db.View(func(tx *bolt.Tx) error { m, ok = get[Msg](tx, bOutbox, id); return nil })
	if !ok {
		return m, ErrNotFound
	}
	return m, nil
}

func (s *Store) ids(bot, state string) ([]string, error) {
	var ms []Msg
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bOutbox).ForEach(func(_, v []byte) error {
			var m Msg
			if json.Unmarshal(v, &m) == nil && m.Bot == bot && m.State == state {
				ms = append(ms, m)
			}
			return nil
		})
	})
	sort.Slice(ms, func(i, j int) bool { return ms[i].Created < ms[j].Created })
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.RequestID
	}
	return out, err
}

// PendingIDs lists never-attempted messages of a bot, oldest first.
func (s *Store) PendingIDs(bot string) ([]string, error) { return s.ids(bot, Pending) }

// UncertainIDs lists messages whose send outcome is unknown.
func (s *Store) UncertainIDs(bot string) ([]string, error) { return s.ids(bot, Uncertain) }

// RecoverStartup marks sends interrupted by a crash as uncertain, to be
// reconciled against Slack history before anything is resent.
func (s *Store) RecoverStartup(bot string) (int, error) {
	ids, err := s.ids(bot, Sending)
	for _, id := range ids {
		if _, err := s.transition(id, Sending, Uncertain, func(m *Msg) { m.Error = "interrupted by restart" }); err != nil {
			return 0, err
		}
	}
	return len(ids), err
}

// MarkSeen records an inbound event key. It returns false if it was
// already seen (Slack retry, reconnect replay or restart).
func (s *Store) MarkSeen(bot, key string) (bool, error) {
	fresh := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		k := bot + "\x00" + key
		if tx.Bucket(bSeen).Get([]byte(k)) != nil {
			return nil
		}
		fresh = true
		return put(tx, bSeen, k, s.ts())
	})
	return fresh, err
}

type revocation struct {
	By string `json:"by"`
	At int64  `json:"at"`
}

// Revoke adds a task id (usually a root) to the revocation list.
func (s *Store) Revoke(taskID, by string) error {
	if taskID == "" {
		return errors.New("empty task id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, ok := get[revocation](tx, bRevoked, taskID); ok {
			return nil
		}
		return put(tx, bRevoked, taskID, revocation{By: by, At: s.ts()})
	})
}

// AnyRevoked implements token.Revoker.
func (s *Store) AnyRevoked(ids ...string) (bool, error) {
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bRevoked)
		for _, id := range ids {
			if b.Get([]byte(id)) != nil {
				found = true
			}
		}
		return nil
	})
	return found, err
}

// Revocations lists revoked task ids, newest first.
func (s *Store) Revocations(limit int) ([]string, error) {
	type rv struct {
		id string
		at int64
	}
	var all []rv
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bRevoked).ForEach(func(k, v []byte) error {
			var r revocation
			_ = json.Unmarshal(v, &r)
			all = append(all, rv{string(k), r.At})
			return nil
		})
	})
	sort.Slice(all, func(i, j int) bool { return all[i].at > all[j].at })
	var out []string
	for i := 0; i < len(all) && i < limit; i++ {
		out = append(out, all[i].id)
	}
	return out, err
}

// Session is a persisted (bot, thread) -> native session binding.
type Session struct {
	NativeID string `json:"native_id,omitempty"`
	RootTask string `json:"root_task,omitempty"`
	// Inflight is the Slack ts of the inbound message whose turn is running;
	// cleared when the turn ends. Non-empty after a crash = resume and tell
	// the harness to check what already happened.
	Inflight       string `json:"inflight,omitempty"`
	InflightSource string `json:"inflight_source,omitempty"`
	InflightUser   string `json:"inflight_user,omitempty"`
	Channel        string `json:"channel,omitempty"`
	ThreadTS       string `json:"thread_ts,omitempty"`
	Updated        int64  `json:"updated"`
}

func sessKey(bot, thread string) string { return bot + "\x00" + thread }

// UpdateSession edits the binding for (bot, thread) atomically.
func (s *Store) UpdateSession(bot, thread string, edit func(*Session)) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v, _ := get[Session](tx, bSessions, sessKey(bot, thread))
		edit(&v)
		v.Updated = s.ts()
		return put(tx, bSessions, sessKey(bot, thread), v)
	})
}

// SaveSession sets the non-empty fields of v.
func (s *Store) SaveSession(bot, thread string, v Session) error {
	return s.UpdateSession(bot, thread, func(cur *Session) {
		if v.NativeID != "" {
			cur.NativeID = v.NativeID
		}
		if v.RootTask != "" {
			cur.RootTask = v.RootTask
		}
		if v.Channel != "" {
			cur.Channel, cur.ThreadTS = v.Channel, v.ThreadTS
		}
	})
}

// LoadSession returns the binding, or ok=false.
func (s *Store) LoadSession(bot, thread string) (Session, bool, error) {
	var v Session
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error { v, ok = get[Session](tx, bSessions, sessKey(bot, thread)); return nil })
	return v, ok, err
}

// Inflight lists the threads of bot whose turn was interrupted.
func (s *Store) Inflight(bot string) (map[string]Session, error) {
	out := map[string]Session{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bSessions).ForEach(func(k, v []byte) error {
			b, thread, _ := strings.Cut(string(k), "\x00")
			var sv Session
			if b == bot && json.Unmarshal(v, &sv) == nil && sv.Inflight != "" {
				out[thread] = sv
			}
			return nil
		})
	})
	return out, err
}

// RootTaskForThread finds the newest root task any bot recorded for a thread.
func (s *Store) RootTaskForThread(thread string) (string, error) {
	var id string
	var at int64
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bSessions).ForEach(func(k, v []byte) error {
			_, th, _ := strings.Cut(string(k), "\x00")
			var sv Session
			if th == thread && json.Unmarshal(v, &sv) == nil && sv.RootTask != "" && sv.Updated >= at {
				id, at = sv.RootTask, sv.Updated
			}
			return nil
		})
	})
	return id, err
}

// Origin records, for every message a partner posted, the trust source of
// the turn that wrote it, so a partner cannot launder a stranger's request.
type Origin struct {
	Bot     string `json:"bot"`
	Source  string `json:"source"`
	Handoff string `json:"handoff,omitempty"`
	At      int64  `json:"at"`
}

// PutOrigin records the origin of the Slack message (channel, ts).
func (s *Store) PutOrigin(channel, ts string, o Origin) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		o.At = s.ts()
		return put(tx, bOrigins, channel+"\x00"+ts, o)
	})
}

// GetOrigin returns the origin of a Slack message, if recorded.
func (s *Store) GetOrigin(channel, ts string) (Origin, bool, error) {
	var o Origin
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error { o, ok = get[Origin](tx, bOrigins, channel+"\x00"+ts); return nil })
	return o, ok, err
}

// Handoff is a stored handoff record (Record is the JSON of handoff.Record).
type Handoff struct {
	TaskID, ParentTask, Channel, Thread, FromBot, ToBot string
	Record                                              json.RawMessage
	At                                                  int64
}

// PutHandoff stores the handoff record of a delegated task (first write wins).
func (s *Store) PutHandoff(h Handoff) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, ok := get[Handoff](tx, bHandoffs, h.TaskID); ok {
			return nil
		}
		h.At = s.ts()
		return put(tx, bHandoffs, h.TaskID, h)
	})
}

// GetHandoff returns the handoff record of a task, if any.
func (s *Store) GetHandoff(taskID string) (Handoff, bool, error) {
	var h Handoff
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error { h, ok = get[Handoff](tx, bHandoffs, taskID); return nil })
	return h, ok, err
}

// Question states.
const (
	QuestionOpen     = "open"
	QuestionAnswered = "answered"
	QuestionLost     = "lost" // the harness process ended before an answer
)

// Question is a harness question posted to a Slack thread.
type Question struct {
	Thread, Bot, QID, SlackTS, State string
	At                               int64
}

func qKey(thread, bot, qid string) string { return thread + "\x00" + bot + "\x00" + qid }

// PutQuestion records or updates a question.
func (s *Store) PutQuestion(q Question) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if q.At == 0 {
			q.At = s.ts()
		}
		return put(tx, bQuestions, qKey(q.Thread, q.Bot, q.QID), q)
	})
}

// SetQuestionState changes a question's state.
func (s *Store) SetQuestionState(thread, bot, qid, state string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		q, ok := get[Question](tx, bQuestions, qKey(thread, bot, qid))
		if !ok {
			return nil
		}
		q.State = state
		return put(tx, bQuestions, qKey(thread, bot, qid), q)
	})
}

// OpenQuestions lists the open questions of a bot.
func (s *Store) OpenQuestions(bot string) ([]Question, error) {
	var out []Question
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bQuestions).ForEach(func(_, v []byte) error {
			var q Question
			if json.Unmarshal(v, &q) == nil && q.Bot == bot && q.State == QuestionOpen {
				out = append(out, q)
			}
			return nil
		})
	})
	return out, err
}

// Prune deletes dedup and origin records older than age.
func (s *Store) Prune(age time.Duration) error {
	cut := s.now().Add(-age).UnixNano()
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bSeen, bOrigins} {
			var old [][]byte
			b := tx.Bucket(name)
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var at int64
				if name[0] == 's' {
					_ = json.Unmarshal(v, &at)
				} else {
					var o Origin
					_ = json.Unmarshal(v, &o)
					at = o.At
				}
				if at < cut {
					old = append(old, append([]byte(nil), k...))
				}
			}
			for _, k := range old { // never delete while iterating a bbolt cursor
				if err := b.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
