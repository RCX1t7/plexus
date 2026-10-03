// Package fakes holds scripted stand-ins for the harness CLIs, used by
// tests through the TestMain re-exec pattern:
//
//	func TestMain(m *testing.M) { fakes.MaybeRun(); os.Exit(m.Run()) }
//
// A test then starts a session with Exe = os.Args[0] and Env
// PLEXUS_FAKE=claude|codex|acp|dsh|tree. Prompts steer the fake:
//
//	TOOL <kind> <path>   ask permission for a tool (read/write/shell)
//	ASK                  ask the user a question
//	RETRY                (codex) send a willRetry error first
//	SLOW                 wait until interrupted
//
// Anything else is echoed back as "echo: <text>".
package fakes

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MaybeRun runs a fake and exits if PLEXUS_FAKE is set.
func MaybeRun() {
	mode := os.Getenv("PLEXUS_FAKE")
	if mode == "" {
		return
	}
	logArgs()
	switch mode {
	case "claude":
		claude()
	case "codex":
		rpcServer(false, codex)
	case "acp":
		rpcServer(true, acp)
	case "dsh":
		rpcServer(true, dsh)
	case "tree":
		tree()
	case "sleep":
		time.Sleep(time.Hour)
	default:
		fmt.Fprintln(os.Stderr, "unknown fake", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

// logArgs writes argv and selected env to PLEXUS_FAKE_LOG for assertions.
func logArgs() {
	p := os.Getenv("PLEXUS_FAKE_LOG")
	if p == "" {
		return
	}
	rec := map[string]any{"args": os.Args[1:], "CLAUDECODE": os.Getenv("CLAUDECODE"),
		"CLAUDE_CODE_SIMPLE": os.Getenv("CLAUDE_CODE_SIMPLE")}
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(p, b, 0o600)
}

// appendLog appends one JSON record to PLEXUS_FAKE_LOG + ".msgs".
func appendLog(v any) {
	p := os.Getenv("PLEXUS_FAKE_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p+".msgs", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(v)
	f.Write(append(b, '\n'))
}

type out struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (o *out) send(v any) {
	b, _ := json.Marshal(v)
	o.mu.Lock()
	o.w.Write(append(b, '\n'))
	o.w.Flush()
	o.mu.Unlock()
}

func lines() *bufio.Scanner {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<16), 1<<24)
	return sc
}

// tree starts a grandchild, reports both pids, then waits.
func tree() {
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "PLEXUS_FAKE=sleep")
	if err := c.Start(); err != nil {
		os.Exit(3)
	}
	o := &out{w: bufio.NewWriter(os.Stdout)}
	o.send(map[string]any{"pid": os.Getpid(), "child": c.Process.Pid})
	for sc := lines(); sc.Scan(); {
	}
	// stdin closed: misbehave on purpose and keep running (Close must kill us)
	time.Sleep(time.Hour)
}

func fields(text string) []string { return strings.Fields(text) }
