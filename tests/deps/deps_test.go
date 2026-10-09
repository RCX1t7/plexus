// Package deps_test enforces the dependency budget: the module's DIRECT
// requires must be only slack-go, bbolt and golang.org/x/sys (Windows job
// objects; review item d). Indirect deps (x/sync, gorilla/websocket) are
// allowed because slack-go pulls them in.
package deps_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var allowedDirect = map[string]bool{
	"github.com/slack-go/slack": true,
	"go.etcd.io/bbolt":          true,
	"golang.org/x/sys":          true,
}

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file))) // tests/deps -> tests -> repo
}

func TestDirectDependencyBudget(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	// Collect every require path NOT marked "// indirect".
	line := regexp.MustCompile(`^\s*([^\s]+)\s+v[^\s]+(\s*//\s*indirect)?\s*$`)
	var direct []string
	inBlock := false
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "require ("):
			inBlock = true
			continue
		case inBlock && t == ")":
			inBlock = false
			continue
		case strings.HasPrefix(t, "require "):
			t = strings.TrimPrefix(t, "require ")
		case !inBlock:
			continue
		}
		m := line.FindStringSubmatch(t)
		if m == nil || m[2] != "" { // no match or indirect
			continue
		}
		direct = append(direct, m[1])
	}
	if len(direct) == 0 {
		t.Fatal("parsed zero direct requires from go.mod; parser or file layout changed")
	}
	for _, d := range direct {
		if !allowedDirect[d] {
			t.Errorf("unexpected DIRECT dependency %q; only slack-go, bbolt and x/sys are allowed as direct requires", d)
		}
	}
	t.Logf("direct requires: %v", direct)
}

// TestModTidy fails when go.mod/go.sum are not tidy, so a dependency marked
// "// indirect" by hand (or a stale require) cannot slip past the budget.
func TestModTidy(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH")
	}
	cmd := exec.Command("go", "mod", "tidy", "-diff")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy -diff: %v\n%s", err, out)
	}
}
