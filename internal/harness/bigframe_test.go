package harness_test

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/harness"
)

// The test binary doubles as a child that writes one oversize frame between
// two normal ones, then waits for stdin to close.
func init() {
	if os.Getenv("PLEXUS_TEST_BIGFRAME") != "1" {
		return
	}
	in := bufio.NewReader(os.Stdin)
	_, _ = in.ReadString('\n') // wait until the parent has set OnDrop
	w := bufio.NewWriter(os.Stdout)
	w.WriteString(`{"n":1}` + "\n")
	w.WriteString(`{"big":"` + strings.Repeat("x", harness.MaxFrame) + `"}` + "\n")
	w.WriteString(`{"n":2}` + "\n")
	w.Flush()
	_, _ = in.ReadString('\n')
	os.Exit(0)
}

func TestOversizeFrameKeepsSession(t *testing.T) {
	p, err := harness.StartProc(context.Background(), os.Args[0], nil, t.TempDir(), []string{"PLEXUS_TEST_BIGFRAME=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	dropped := make(chan int64, 1)
	p.OnDrop(func(n int64) { dropped <- n })
	if err := p.Write(map[string]int{"go": 1}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 2 {
		select {
		case l, ok := <-p.Lines:
			if !ok {
				t.Fatalf("stream ended after %v", got)
			}
			got = append(got, string(l))
		case <-time.After(20 * time.Second):
			t.Fatalf("timeout after %v", got)
		}
	}
	if got[0] != `{"n":1}` || got[1] != `{"n":2}` {
		t.Fatal(got)
	}
	select {
	case n := <-dropped:
		if n <= harness.MaxFrame {
			t.Fatal(n)
		}
	case <-time.After(time.Second):
		t.Fatal("no drop reported")
	}
	select {
	case <-p.Done():
		t.Fatal("process exited")
	default:
	}
	if ev := harness.DroppedFrame(5); ev.Kind != harness.EventError || ev.TurnID != "" {
		t.Fatal(ev)
	}
}
