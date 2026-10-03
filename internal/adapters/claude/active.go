package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/platform"
)

// recentWrite is how fresh a transcript write by someone else must be to
// count as "in use".
const recentWrite = 2 * time.Minute

// ActiveElsewhere reports whether Claude session id is open in another
// process. Claude Code has no session lock ("messages from both interleave"),
// so Plexus checks, best effort and read-only:
//   - <claude dir>/sessions/*.json, one small record per running session:
//     a record naming id with a live pid means it is running;
//   - the transcript projects/*/<id>.jsonl written in the last 2 minutes,
//     after Plexus's own last use.
//
// The record format is undocumented; unreadable records are skipped.
func (Adapter) ActiveElsewhere(env harness.Env, id string, ourLast time.Time) (string, bool) {
	if id == "" {
		return "", false
	}
	dir := env.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(env.Home, ".claude")
	}
	recs, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	for _, p := range recs {
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 1<<20 {
			continue
		}
		var r struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(b, &r) == nil && r.SessionID == id && r.PID != os.Getpid() && platform.ProcessAlive(r.PID) {
			return fmt.Sprintf("Claude Code process %d has it open (Claude Desktop or a terminal)", r.PID), true
		}
	}
	ts, _ := filepath.Glob(filepath.Join(dir, "projects", "*", id+".jsonl"))
	for _, p := range ts {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		m := fi.ModTime()
		if age := time.Since(m); age < recentWrite && m.After(ourLast.Add(10*time.Second)) {
			return fmt.Sprintf("its transcript was written %ds ago by another Claude Code (Desktop or a terminal)", int(age.Seconds())), true
		}
	}
	return "", false
}
