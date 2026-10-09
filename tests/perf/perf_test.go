// Package perf_test holds the performance / size gates against the REAL
// plexus binary and the real store. Numbers are measured on Linux; the
// 25 MiB size gate is the windows/amd64 release build. Windows startup/RSS
// must be confirmed on Sin's laptop (see docs/VERIFICATION.md).
package perf_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/internal/store"
)

const sizeGateMiB = 25

func repoRoot(tb testing.TB) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// buildRelease builds cmd/plexus for goos/amd64 with the release flags.
func buildRelease(tb testing.TB, goos string) string {
	tb.Helper()
	out := filepath.Join(tb.TempDir(), "plexus-"+goos)
	if goos == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/plexus")
	cmd.Dir = repoRoot(tb)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("build %s: %v\n%s", goos, err, b)
	}
	return out
}

func TestBinarySizeWindowsAMD64(t *testing.T) {
	bin := buildRelease(t, "windows")
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	mib := float64(fi.Size()) / (1 << 20)
	t.Logf("windows/amd64 plexus.exe = %.2f MiB (gate <= %d MiB)", mib, sizeGateMiB)
	if fi.Size() > int64(sizeGateMiB)<<20 {
		t.Errorf("binary %.2f MiB exceeds %d MiB gate", mib, sizeGateMiB)
	}
}

func TestVersionColdStart(t *testing.T) {
	bin := buildRelease(t, runtime.GOOS)
	// warm the page cache
	_ = exec.Command(bin, "--version").Run()
	const n = 15
	var best time.Duration = time.Hour
	var sum time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
			t.Fatalf("run --version: %v\n%s", err, out)
		}
		d := time.Since(start)
		sum += d
		if d < best {
			best = d
		}
	}
	t.Logf("plexus --version: best=%v avg=%v over %d runs (gate <= 50ms, Linux; Windows TBD on laptop)",
		best.Round(time.Microsecond), (sum / n).Round(time.Microsecond), n)
	if best > 50*time.Millisecond {
		t.Errorf("cold start best %v exceeds 50ms", best)
	}
}

// BenchmarkDurableDedupWrites measures the durable bbolt write path
// (MarkSeen = one fsync'd transaction per inbound event key). Gate: >= 2000/s.
func BenchmarkDurableDedupWrites(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "plexus.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if _, err := st.MarkSeen("bot", fmt.Sprintf("Ev%08d", i)); err != nil {
			b.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	b.StopTimer()
	rate := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(rate, "writes/s")
	if b.N >= 2000 && rate < 2000 {
		b.Errorf("durable writes %.0f/s are below the 2000/s gate", rate)
	}
}

// BenchmarkDedupThroughput measures the dedup check+write throughput
// (durable, ~50% duplicates). Informational only: dedup has no gate. The
// 20k ev/s gate is in-memory routing (internal/slackbot
// BenchmarkEventRouting); the disk gate is BenchmarkDurableDedupWrites.
func BenchmarkDedupThroughput(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "plexus.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	// pre-seed half the keys so ~50% are duplicates (realistic reconnect)
	for i := 0; i < 1000; i++ {
		_, _ = st.MarkSeen("bot", fmt.Sprintf("K%06d", i))
	}
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		_, _ = st.MarkSeen("bot", fmt.Sprintf("K%06d", i%2000))
	}
	rate := float64(b.N) / time.Since(start).Seconds()
	b.ReportMetric(rate, "ev/s")
}
