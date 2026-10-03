package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

func TestActiveElsewhere(t *testing.T) {
	dir := t.TempDir()
	env := harness.Env{GOOS: "linux", Home: "/nonexistent", Getenv: func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return dir
		}
		return ""
	}}
	a := Adapter{}
	if _, busy := a.ActiveElsewhere(env, "s1", time.Time{}); busy {
		t.Fatal("busy with nothing on disk")
	}
	// a live process records the session
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	_ = os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	rec := filepath.Join(dir, "sessions", "1.json")
	_ = os.WriteFile(rec, []byte(`{"pid":`+strconv.Itoa(cmd.Process.Pid)+`,"sessionId":"s1","cwd":"/w"}`), 0o644)
	if d, busy := a.ActiveElsewhere(env, "s1", time.Time{}); !busy || d == "" {
		t.Fatal("live record not seen")
	}
	if _, busy := a.ActiveElsewhere(env, "s2", time.Time{}); busy {
		t.Fatal("other session reported busy")
	}
	_ = os.WriteFile(rec, []byte(`{"pid":999999999,"sessionId":"s1"}`), 0o644)
	if _, busy := a.ActiveElsewhere(env, "s1", time.Time{}); busy {
		t.Fatal("dead pid counted")
	}
	// a fresh transcript write after our own last use
	_ = os.MkdirAll(filepath.Join(dir, "projects", "-w"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "projects", "-w", "s1.jsonl"), []byte("{}\n"), 0o644)
	if _, busy := a.ActiveElsewhere(env, "s1", time.Now().Add(-time.Hour)); !busy {
		t.Fatal("fresh foreign transcript write not seen")
	}
	if _, busy := a.ActiveElsewhere(env, "s1", time.Now()); busy {
		t.Fatal("our own recent write counted")
	}
}
