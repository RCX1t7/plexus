package slackbot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/platform"
)

// checkActive runs the harness's own "open elsewhere?" check before a
// resume (harnesses with a native lock report it from StartSession).
func (w *Worker) checkActive(t *thread, resumeID string) error {
	ac, ok := w.Harness.(harness.ActivityChecker)
	if !ok || resumeID == "" {
		return nil
	}
	var last time.Time
	if s, ok, _ := w.Store.LoadSession(w.Bot.Name, t.key); ok && s.Updated > 0 {
		last = time.Unix(0, s.Updated)
		if s.Inflight != "" {
			// Plexus died mid-turn: recent transcript writes are our own
			// child's, so only the live-process check applies.
			last = time.Now()
		}
	}
	if detail, busy := ac.ActiveElsewhere(harness.OSEnv(), resumeID, last); busy {
		return fmt.Errorf("%w: %s", harness.ErrActiveElsewhere, detail)
	}
	return nil
}

// busyText is the single message posted when a session is open elsewhere.
func busyText(err error) string {
	return "⏸️ I did not resume this conversation: " + strings.TrimPrefix(err.Error(), harness.ErrActiveElsewhere.Error()+": ") +
		". Close it there (or let it go idle), then send your message again. Nothing was written to it, so nothing got interleaved."
}

func isBusy(err error) bool { return errors.Is(err, harness.ErrActiveElsewhere) }

// idleFor is how long an idle thread keeps its harness process.
func (w *Worker) idleFor() time.Duration {
	if u, ok := w.Harness.(harness.IdleUnloader); ok && u.IdleUnload() > 0 {
		return u.IdleUnload()
	}
	return idleClose
}

// workdir notes: <LockDir>/workdir-<sha256(canonical path)>.lock records who
// works where. It is a warning only, never an exclusive lock: partners may
// share a repo. Sessions are already serialized per thread in-process.
type workdirNote struct {
	PID     int    `json:"pid"`
	Partner string `json:"partner"`
	Thread  string `json:"thread"`
	Started int64  `json:"started"`
}

func (w *Worker) workdirLock() string {
	if w.LockDir == "" || w.Policy.Workdir == "" {
		return ""
	}
	p, err := filepath.Abs(w.Policy.Workdir)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	sum := sha256.Sum256([]byte(p))
	return filepath.Join(w.LockDir, "workdir-"+hex.EncodeToString(sum[:])+".lock")
}

func (w *Worker) noteWorkdir(t *thread) {
	path := w.workdirLock()
	if path == "" {
		return
	}
	var old workdirNote
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &old) == nil &&
		(old.Partner != w.Bot.Name || old.Thread != t.key) && platform.ProcessAlive(old.PID) {
		w.log().Warn("workdir is also in use (warning only; sharing is allowed)", "workdir", w.Policy.Workdir,
			"other_partner", old.Partner, "other_thread", old.Thread, "other_pid", old.PID)
	}
	b, _ := json.Marshal(workdirNote{PID: os.Getpid(), Partner: w.Bot.Name, Thread: t.key, Started: time.Now().Unix()})
	_ = os.MkdirAll(w.LockDir, 0o700)
	_ = os.WriteFile(path, b, 0o600)
}

func (w *Worker) releaseWorkdir(t *thread) {
	path := w.workdirLock()
	if path == "" {
		return
	}
	var cur workdirNote
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cur) == nil &&
		cur.Partner == w.Bot.Name && cur.Thread == t.key && cur.PID == os.Getpid() {
		_ = os.Remove(path)
	}
}
