package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/fakes"
	"github.com/RCX1t7/plexus/internal/harness"
)

func TestMain(m *testing.M) { fakes.MaybeRun(); os.Exit(m.Run()) }

// alive reports a live (non-zombie) process on Linux.
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return len(f) > 0 && f[0] != "Z"
}

func TestCloseKillsWholeProcessTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses /proc")
	}
	p, err := harness.StartProc(context.Background(), os.Args[0], nil, t.TempDir(), []string{"PLEXUS_FAKE=tree"})
	if err != nil {
		t.Fatal(err)
	}
	var pids struct{ Pid, Child int }
	select {
	case line := <-p.Lines:
		if err := json.Unmarshal(line, &pids); err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no pid line")
	}
	if !alive(pids.Pid) || !alive(pids.Child) {
		t.Fatal("tree not running")
	}
	// The fake ignores stdin EOF on purpose; Close must kill the group.
	_ = p.Close()
	deadline := time.Now().Add(10 * time.Second)
	for (alive(pids.Pid) || alive(pids.Child)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pids.Pid) || alive(pids.Child) {
		t.Fatalf("survivors: parent=%v child=%v", alive(pids.Pid), alive(pids.Child))
	}
}
