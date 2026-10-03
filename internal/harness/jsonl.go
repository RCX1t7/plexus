package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/platform"
)

// Proc is a child process spoken to with newline-delimited JSON on stdio.
// All three first adapters (stream-json, app-server, ACP) use it.
type Proc struct {
	cmd       *exec.Cmd
	tree      *platform.Tree
	stdin     io.WriteCloser
	wmu       sync.Mutex
	Lines     <-chan json.RawMessage // closed when stdout ends
	done      chan struct{}
	waitMu    sync.Mutex
	err       error
	closeOnce sync.Once
}

// StartProc launches exe with args in dir. stderr is drained and discarded
// (it can contain prompts or tokens) unless PLEXUS_HARNESS_STDERR=1. stdout
// lines may be large (tool results); up to 32 MiB per line is accepted.
func StartProc(ctx context.Context, exe string, args []string, dir string, env []string) (*Proc, error) {
	if exe == "" {
		return nil, errors.New("harness executable not found")
	}
	name, argv, err := Command(exe, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(name, argv...)
	cmd.Dir = dir
	cmd.Env = ChildEnv(env)
	platform.PrepareTree(cmd)
	if os.Getenv("PLEXUS_HARNESS_STDERR") == "1" {
		cmd.Stderr = os.Stderr
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", exe, err)
	}
	tree, err := platform.AttachTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("attach process tree: %w", err)
	}
	lines := make(chan json.RawMessage, 64)
	p := &Proc{cmd: cmd, tree: tree, stdin: in, Lines: lines, done: make(chan struct{})}
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
		for sc.Scan() {
			b := sc.Bytes()
			if len(b) == 0 || b[0] != '{' {
				continue // tolerate stray non-JSON output
			}
			lines <- append(json.RawMessage(nil), b...)
		}
	}()
	go func() {
		err := cmd.Wait()
		p.waitMu.Lock()
		p.err = err
		p.waitMu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// Write sends one JSON message followed by a newline.
func (p *Proc) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Done is closed when the process exits.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Close closes stdin, gives the harness a moment to exit, then kills the
// whole process tree (grandchildren such as shells and MCP servers too).
func (p *Proc) Close() error {
	p.closeOnce.Do(func() {
		p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
		_ = p.tree.Kill()
		<-p.done
	})
	return nil
}

// Pid returns the child's process id.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }
