package acceptance

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/RCX1t7/plexus/tests/simkit"
)

var fakeBin, refhubBin string

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Println("scripted simulation runs on Linux only (process-table + process-group checks); use the Windows checklist in ACCEPTANCE.md §B")
		os.Exit(0)
	}
	// Building the fake harness + reference hub is only needed for the heavy
	// refhub self-check suite (PLEXUS_SELFCHECK=1) or the real-hub sim
	// (PLEXUS_BIN set). Default `go test ./...` skips both and stays fast.
	dir := ""
	if os.Getenv("PLEXUS_SELFCHECK") == "1" || os.Getenv("PLEXUS_BIN") != "" {
		var err error
		dir, err = os.MkdirTemp("", "plexus-sim-bin")
		if err != nil {
			panic(err)
		}
		fakeBin, refhubBin, err = simkit.BuildBinaries(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

func newEnv(t testing.TB, spec simkit.HubSpec) *simkit.Env {
	t.Helper()
	return newEnvOpts(t, spec, simkit.Opts{})
}

func newEnvOpts(t testing.TB, spec simkit.HubSpec, opts simkit.Opts) *simkit.Env {
	t.Helper()
	e, err := simkit.NewEnvOpts(t.TempDir(), spec, fakeBin, opts, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// skipSelfCheck skips the heavy refhub self-check suite unless explicitly
// enabled. The self-checks validate THIS suite (against a reference stub),
// not Plexus, so they are opt-in and kept out of the default CI race run.
func skipSelfCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("PLEXUS_SELFCHECK") != "1" {
		t.Skip("set PLEXUS_SELFCHECK=1 to run the refhub self-check suite")
	}
}
