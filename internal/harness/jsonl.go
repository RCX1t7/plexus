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
	"sync/atomic"
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
	onDrop    atomic.Pointer[func(int64)]
}

// MaxFrame is the largest stdout line a harness may send. A longer line is
// discarded up to its newline and reported through OnDrop; the session goes on.
const MaxFrame = 32 * 1024 * 1024

// OnDrop sets the callback for discarded oversize frames (size in bytes).
func (p *Proc) OnDrop(fn func(size int64)) { p.onDrop.Store(&fn) }

func (p *Proc) dropped(n int64) {
	if fn := p.onDrop.Load(); fn != nil && *fn != nil {
		(*fn)(n)
	}
}

// DroppedFrame is the session-level error event adapters emit from OnDrop.
func DroppedFrame(size int64) Event {
	return Event{Kind: EventError, Status: "frame_dropped",
		Text: fmt.Sprintf("dropped one %d-byte message from the harness (limit %d bytes)", size, MaxFrame)}
}

// readFrames calls line for every newline-terminated frame of at most max
// bytes (newline and a trailing CR excluded) and drop for every longer one,
// which it skips without buffering it.
func readFrames(r io.Reader, max int, line func([]byte), drop func(int64)) {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf []byte
	var over int64 // >0 while skipping an oversize frame
	for {
		chunk, err := br.ReadSlice('\n')
		end := err == nil
		if end {
			chunk = chunk[:len(chunk)-1]
		}
		switch {
		case over > 0:
			over += int64(len(chunk))
		case len(buf)+len(chunk) > max+1: // +1: room for a CR
			over = int64(len(buf) + len(chunk))
			buf = buf[:0]
		default:
			buf = append(buf, chunk...)
		}
		if end || (err != nil && err != bufio.ErrBufferFull) {
			if over > 0 {
				drop(over)
			} else {
				b := buf
				if n := len(b); n > 0 && b[n-1] == '\r' {
					b = b[:n-1]
				}
				if len(b) > max {
					drop(int64(len(b)))
				} else if len(b) > 0 {
					line(b)
				}
			}
			buf, over = buf[:0], 0
			if cap(buf) > 1<<20 {
				buf = nil // do not pin a huge buffer after one big frame
			}
		}
		if err != nil && err != bufio.ErrBufferFull {
			return
		}
	}
}

// StartProc launches exe with args in dir. stderr is drained and discarded
// (it can contain prompts or tokens) unless PLEXUS_HARNESS_STDERR=1. stdout
// lines may be large (tool results); up to MaxFrame per line is accepted.
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
		readFrames(out, MaxFrame, func(b []byte) {
			if len(b) == 0 || b[0] != '{' {
				return // tolerate stray non-JSON output
			}
			lines <- append(json.RawMessage(nil), b...)
		}, p.dropped)
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
		select { // bounded: stop must clear within its time budget
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
	})
	return nil
}

// Pid returns the child's process id.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }
