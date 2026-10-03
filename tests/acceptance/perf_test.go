package acceptance

// Performance gates (ACCEPTANCE.md §P). Sin-approved thresholds (answer #16):
//   exe ≤ 25 MiB, `plexus --version` ≤ 50 ms, `plexus run` ready ≤ 500 ms on
//   fake Slack, 3 partners idle ≤ 30 MiB & ≤ 1% CPU, internal throughput
//   ≥ 20k ev/s, durable writes ≥ 2000/s.
//
// Target selection:
//   real hub : PLEXUS_BIN set -> thresholds asserted.
//   refhub   : HH_PERF_TARGET=refhub -> measuring code exercised, numbers
//              logged, never asserted (says nothing about Plexus).

import (
	"debug/pe"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RCX1t7/plexus/tests/fakeharness"
	"github.com/RCX1t7/plexus/tests/fakeslack"
	"github.com/RCX1t7/plexus/tests/simkit"
)

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func perfTarget(t testing.TB) (simkit.HubSpec, bool) {
	if spec, skip := simkit.HubSpecFromEnv(); skip == "" {
		return spec, true
	}
	if os.Getenv("HH_PERF_TARGET") == "refhub" {
		return simkit.RefHubSpec(refhubBin), false
	}
	t.Skip("performance target missing: set PLEXUS_BIN for the real hub (or HH_PERF_TARGET=refhub to exercise the measuring code against the stub)")
	return simkit.HubSpec{}, false
}

// P1-b throughput (black box): one fake harness floods b.N events.
func BenchmarkHubEventThroughput(b *testing.B) {
	spec, _ := perfTarget(b)
	e := newEnv(b, spec)
	if err := e.Hub.Start(); err != nil {
		b.Fatal(err)
	}
	if _, _, _, ok := e.Hub.WaitReady(10 * time.Second); !ok {
		b.Fatal("hub did not become ready")
	}
	b.ResetTimer()
	start := time.Now()
	root := e.Slack.UserPost("", fmt.Sprintf("<@%s> %s %d]", e.UserID("claude"), fakeharness.ModeFlood, b.N))
	ok := e.Slack.WaitFor(time.Duration(b.N)*10*time.Millisecond+20*time.Second, func(ms []fakeslack.Message) bool {
		for _, m := range ms {
			if m.ThreadTS == root && strings.Contains(m.Text, fakeharness.TagFloodDone) {
				return true
			}
		}
		return false
	})
	el := time.Since(start)
	b.StopTimer()
	if !ok {
		b.Fatalf("flood of %d events did not complete", b.N)
	}
	b.ReportMetric(float64(b.N)/el.Seconds(), "events/s")
}

// P2 idle RSS / CPU: 3 partners connected and idle.
func TestPerfIdleResources(t *testing.T) {
	spec, assert := perfTarget(t)
	e := newEnv(t, spec)
	if err := e.Hub.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := e.Hub.WaitReady(10 * time.Second); !ok {
		t.Fatal("hub did not become ready")
	}
	time.Sleep(time.Duration(envFloat("HH_PERF_WARMUP_SECONDS", 3) * float64(time.Second)))
	secs := envFloat("HH_PERF_IDLE_SECONDS", 8)
	pid := e.Hub.PID()
	p0, ok := simkit.ReadProc(pid)
	if !ok {
		t.Fatal("hub process vanished")
	}
	t0 := time.Now()
	var peak int64
	var rss []int64
	for time.Since(t0) < time.Duration(secs*float64(time.Second)) {
		p, _ := simkit.ReadProc(pid)
		rss = append(rss, p.RSSKiB)
		if p.RSSKiB > peak {
			peak = p.RSSKiB
		}
		time.Sleep(time.Second)
	}
	p1, _ := simkit.ReadProc(pid)
	cpu := float64((p1.UTime+p1.STime)-(p0.UTime+p0.STime)) / simkit.ClockTicks / time.Since(t0).Seconds() * 100
	sort.Slice(rss, func(i, j int) bool { return rss[i] < rss[j] })
	med := float64(rss[len(rss)/2]) / 1024
	maxRSS, maxCPU := envFloat("HH_PERF_MAX_RSS_MIB", 30), envFloat("HH_PERF_MAX_CPU_PCT", 1.0)
	t.Logf("[%s] idle %.0fs: RSS median %.1f MiB, peak %.1f MiB, CPU avg %.2f%% (gates: RSS ≤ %.0f MiB, CPU ≤ %.1f%%)",
		spec.Name, secs, med, float64(peak)/1024, cpu, maxRSS, maxCPU)
	if !assert {
		t.Log("measure-only (stub target): thresholds not asserted")
		return
	}
	if float64(peak)/1024 > maxRSS || cpu > maxCPU {
		t.Fatalf("idle budget exceeded: peak RSS %.1f MiB (≤%.0f), CPU %.2f%% (≤%.1f)", float64(peak)/1024, maxRSS, cpu, maxCPU)
	}
}

// P4 startup: `plexus --version` cold exec (median of 10) and run->ready (median of 5).
func TestPerfStartupTime(t *testing.T) {
	spec, assert := perfTarget(t)
	vargs := strings.Fields(os.Getenv("PLEXUS_VERSION_ARGS"))
	if len(vargs) == 0 {
		vargs = []string{"--version"}
	}
	var execs []time.Duration
	for i := 0; i < 10; i++ {
		st := time.Now()
		if out, err := exec.Command(spec.Bin, vargs...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", spec.Bin, vargs, err, out)
		}
		execs = append(execs, time.Since(st))
	}
	var readies []time.Duration
	for i := 0; i < 5; i++ {
		e := newEnv(t, spec)
		st := time.Now()
		if err := e.Hub.Start(); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok := e.Hub.WaitReady(10 * time.Second); !ok {
			t.Fatal("hub never became ready")
		}
		readies = append(readies, time.Since(st))
		e.Close()
	}
	med := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	maxExec := time.Duration(envFloat("HH_PERF_MAX_VERSION_MS", 50)) * time.Millisecond
	maxReady := time.Duration(envFloat("HH_PERF_MAX_READY_MS", 500)) * time.Millisecond
	t.Logf("[%s] --version exec median %s (≤%s); run->ready median %s (≤%s)",
		spec.Name, med(execs), maxExec, med(readies), maxReady)
	if !assert {
		t.Log("measure-only (stub target): thresholds not asserted")
		return
	}
	if med(execs) > maxExec || med(readies) > maxReady {
		t.Fatalf("startup budget exceeded")
	}
}

// P3 binary size: cross-build the REAL plexus for windows/amd64 with the exact
// gate flags and check ≤ 25 MiB, static (CGO_ENABLED=0), system DLLs only.
var allowedDLLs = map[string]bool{"kernel32.dll": true, "ws2_32.dll": true, "advapi32.dll": true,
	"winmm.dll": true, "ntdll.dll": true, "user32.dll": true, "shell32.dll": true, "iphlpapi.dll": true,
	"userenv.dll": true, "secur32.dll": true, "crypt32.dll": true, "dnsapi.dll": true, "netapi32.dll": true,
	"bcrypt.dll": true, "bcryptprimitives.dll": true, "mswsock.dll": true, "ole32.dll": true, "psapi.dll": true}

type exeReport struct {
	SizeMiB float64
	CGO0    bool
	Imports []string
	Foreign []string
}

func checkWindowsExe(t *testing.T, srcDir, pkg string) exeReport {
	out := filepath.Join(t.TempDir(), "plexus.exe")
	cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-ldflags", "-s -w", "-o", out, pkg)
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross build failed: %v\n%s", err, b)
	}
	st, _ := os.Stat(out)
	r := exeReport{SizeMiB: float64(st.Size()) / (1 << 20)}
	vb, _ := exec.Command("go", "version", "-m", out).CombinedOutput()
	r.CGO0 = strings.Contains(string(vb), "CGO_ENABLED=0")
	f, err := pe.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	syms, err := f.ImportedSymbols()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range syms {
		i := strings.LastIndexByte(s, ':')
		if i < 0 {
			continue
		}
		l := strings.ToLower(s[i+1:])
		if seen[l] {
			continue
		}
		seen[l] = true
		r.Imports = append(r.Imports, l)
		if !allowedDLLs[l] {
			r.Foreign = append(r.Foreign, l)
		}
	}
	sort.Strings(r.Imports)
	return r
}

func TestPerfBinarySize(t *testing.T) {
	src := os.Getenv("PLEXUS_SRC")
	if src == "" {
		src = "/workspace/plexus"
	}
	pkg := os.Getenv("PLEXUS_MAIN")
	if pkg == "" {
		pkg = "./cmd/plexus"
	}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
		t.Skipf("plexus source not ready (%s/go.mod missing); set PLEXUS_SRC/PLEXUS_MAIN", src)
	}
	if _, err := os.Stat(filepath.Join(src, pkg)); err != nil {
		t.Skipf("main package %s not found in %s; set PLEXUS_MAIN", pkg, src)
	}
	r := checkWindowsExe(t, src, pkg)
	maxMiB := envFloat("HH_PERF_MAX_EXE_MIB", 25)
	t.Logf("plexus.exe: %.2f MiB (≤%.0f), CGO_ENABLED=0: %v, imports: %v", r.SizeMiB, maxMiB, r.CGO0, r.Imports)
	if r.SizeMiB > maxMiB || !r.CGO0 || len(r.Foreign) > 0 {
		t.Fatalf("binary check failed: size %.2f MiB, cgo0=%v, non-system DLLs=%v", r.SizeMiB, r.CGO0, r.Foreign)
	}
}

// TestBinaryCheckerSelf validates the P3 checker on a portable helper binary.
func TestBinaryCheckerSelf(t *testing.T) {
	wd, _ := os.Getwd()
	r := checkWindowsExe(t, filepath.Dir(wd), "./cmd/fakeslack")
	t.Logf("fakeslack.exe: %.2f MiB, CGO_ENABLED=0: %v, imports: %v (checker validation only)", r.SizeMiB, r.CGO0, r.Imports)
	if !r.CGO0 || len(r.Foreign) > 0 || r.SizeMiB <= 0 || len(r.Imports) == 0 {
		t.Fatalf("checker broken: %+v", r)
	}
}
