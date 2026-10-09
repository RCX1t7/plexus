package slackbot

import (
	"context"
	"github.com/RCX1t7/plexus/internal/store"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/policy"
)

// StopGrace is how long a stop waits between the native interrupt and the
// process-tree kill.
var StopGrace = 3 * time.Second

// Stops is the hub-wide registry of which threads (of any partner) work on
// which task tree, so one stop reaches every partner's session.
type Stops struct {
	mu      sync.Mutex
	m       map[string]map[*thread]bool
	workers map[string]*Worker // partner name -> worker (card owners, cross-partner notes)
}

// Register makes w reachable by the other partners' workers (approval
// cards it owns, notes to its threads, stops of trees it works on).
func (s *Stops) Register(w *Worker) {
	if s == nil || w == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers == nil {
		s.workers = map[string]*Worker{}
	}
	s.workers[w.Bot.Name] = w
}

func (s *Stops) worker(name string) *Worker {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[name]
}

func (s *Stops) anyWorker() *Worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		return w
	}
	return nil
}

func (s *Stops) add(root string, t *thread) {
	if s == nil || root == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]map[*thread]bool{}
	}
	if s.m[root] == nil {
		s.m[root] = map[*thread]bool{}
	}
	s.m[root][t] = true
}

func (s *Stops) take(root string) []*thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*thread
	for t := range s.m[root] {
		out = append(out, t)
	}
	delete(s.m, root)
	return out
}

// Stop ends a task tree: 1) record the revocation, 2) stop known background
// tasks and send the native interrupt to every session in the tree,
// 3) wait StopGrace, 4) close the sessions, killing their process trees
// (Job Object / process group). It returns the number of sessions reached.
func (s *Stops) Stop(ctx context.Context, rev interface{ Revoke(id, by string) error }, root, by string) (int, error) {
	if err := rev.Revoke(root, by); err != nil {
		return 0, err
	}
	if w := s.anyWorker(); w != nil {
		// From the store, not the live registry: this also reaches cards
		// parked before a restart, of any partner in the tree.
		cancelRoot(ctx, w, root)
	}
	ts := s.take(root)
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		go func(t *thread) {
			defer wg.Done()
			t.softStop(ctx)
		}(t)
	}
	wg.Wait()
	if len(ts) > 0 {
		select {
		case <-time.After(StopGrace):
		case <-ctx.Done():
		}
	}
	for _, t := range ts {
		t.closeSession()
	}
	return len(ts), nil
}

// softStop asks politely: per-task stop for background tasks, then the
// native interrupt, and cancels the running turn.
func (t *thread) softStop(ctx context.Context) {
	t.mu.Lock()
	s, cancel, qs := t.sess, t.cancel, t.questions
	t.stopped, t.questions, t.queue = true, nil, nil
	t.mu.Unlock()
	for _, q := range qs {
		_ = t.w.Store.SetQuestionState(t.key, t.w.Bot.Name, q.qid, store.QuestionLost)
		if q.ev.Answer != nil {
			q.ev.Answer(nil)
		}
	}
	if s != nil {
		ictx, done := context.WithTimeout(ctx, StopGrace)
		if ts, ok := s.(harness.TaskStopper); ok {
			for _, id := range ts.BackgroundTasks() {
				_ = ts.StopTask(ictx, id)
			}
		}
		_ = s.Interrupt(ictx)
		done()
	}
	if cancel != nil {
		cancel()
	}
}

// StopAck is posted (exactly once per stop) when a task tree is stopped.
const StopAck = "已停止 / stopped"

// stopTree runs a stop requested in Slack and posts the single ack.
func (w *Worker) stopTree(ctx context.Context, root string, in Inbound) {
	if root == "" {
		root = in.Channel + ":" + in.thread()
	}
	n, err := w.Stops.Stop(ctx, w.Store, root, in.User)
	if err != nil {
		w.log().Error("stop failed", "root", root, "err", err.Error())
		return
	}
	w.log().Info("task tree stopped", "root", root, "sessions", n)
	key := in.Channel + ":" + in.thread()
	t := w.thread(ctx, key, in)
	// The request id has no partner name in it: when several partners see
	// the same stop message, the outbox keeps only the first ack.
	w.post(t, job{auth: policy.Authority{Source: policy.FromSin}}, RequestID("stop_ack", in.Channel, in.TS), "stop_ack", StopAck)
}
