package policy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

type revoker map[string]bool

func (r revoker) AnyRevoked(ids ...string) (bool, error) {
	if r == nil {
		return false, errors.New("down")
	}
	for _, id := range ids {
		if r[id] {
			return true, nil
		}
	}
	return false, nil
}

func setup(t *testing.T) (Policy, string) {
	home := t.TempDir()
	wd := filepath.Join(home, "work")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	return Policy{GOOS: runtime.GOOS, Home: home, Workdir: wd, StrangerGuard: true, Revoker: revoker{}}, wd
}

var (
	sin      = Authority{Source: FromSin, Root: "C1:1"}
	partner  = Authority{Source: FromPartner, Root: "C1:1"}
	stranger = Authority{Source: FromStranger, Root: "C1:1"}
)

func TestTrustedMayDoEverything(t *testing.T) {
	p, _ := setup(t)
	for _, a := range []Authority{sin, partner} {
		for _, k := range []harness.ToolKind{harness.ToolShell, harness.ToolWrite, harness.ToolFetch, harness.ToolOther} {
			if d := p.Decide(a, harness.ToolRequest{Kind: k, Paths: []string{"/etc/passwd"}}); !d.Allow {
				t.Fatalf("%s %s denied: %s", a.Source, k, d.Reason)
			}
		}
		if p.Level(a) != harness.LevelFull {
			t.Fatal("trusted level must be full")
		}
	}
}

func TestStrangerOnlyReadsOrdinaryWorkdirFiles(t *testing.T) {
	p, wd := setup(t)
	for _, k := range []harness.ToolKind{harness.ToolShell, harness.ToolWrite, harness.ToolFetch, harness.ToolOther} {
		if d := p.Decide(stranger, harness.ToolRequest{Kind: k}); d.Allow {
			t.Fatalf("stranger %s allowed", k)
		}
	}
	if p.Level(stranger) != harness.LevelReadOnly {
		t.Fatal("stranger level must be read-only")
	}
	allowed := []string{"main.go", filepath.Join(wd, "docs", "a.md"), "."}
	for _, path := range allowed {
		if d := p.Decide(stranger, harness.ToolRequest{Kind: harness.ToolRead, Paths: []string{path}}); !d.Allow {
			t.Fatalf("read %s denied: %s", path, d.Reason)
		}
	}
	denied := []string{".env", "config/.env.local", "prod.env", "id_rsa", "certs/server.pem", "secrets/x.txt",
		"data/users.csv", "auth.json", "../outside.txt", filepath.Join(p.Home, ".ssh", "id_ed25519"), "/etc/passwd"}
	for _, path := range denied {
		if d := p.Decide(stranger, harness.ToolRequest{Kind: harness.ToolRead, Paths: []string{path}}); d.Allow {
			t.Fatalf("read %s allowed", path)
		}
	}
	// meta and ask have no side effects
	if d := p.Decide(stranger, harness.ToolRequest{Kind: harness.ToolMeta}); !d.Allow {
		t.Fatal("meta denied")
	}
}

func TestSymlinkCannotEscapeWorkdir(t *testing.T) {
	p, wd := setup(t)
	secret := filepath.Join(p.Home, "outside.txt")
	_ = os.WriteFile(secret, []byte("x"), 0o600)
	if err := os.Symlink(secret, filepath.Join(wd, "link.txt")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if d := p.Decide(stranger, harness.ToolRequest{Kind: harness.ToolRead, Paths: []string{"link.txt"}}); d.Allow {
		t.Fatal("symlink escape allowed")
	}
}

func TestGuardOff(t *testing.T) {
	p, _ := setup(t)
	p.StrangerGuard = false
	if d := p.Decide(stranger, harness.ToolRequest{Kind: harness.ToolShell}); !d.Allow {
		t.Fatal("guard off: stranger should be trusted")
	}
}

func TestStoppedTreeDeniedFailClosed(t *testing.T) {
	p, _ := setup(t)
	p.Revoker = revoker{"C1:1": true}
	if d := p.Decide(sin, harness.ToolRequest{Kind: harness.ToolMeta}); d.Allow {
		t.Fatal("stopped tree allowed")
	}
	p.Revoker = revoker(nil) // store error
	if d := p.Decide(sin, harness.ToolRequest{Kind: harness.ToolRead}); d.Allow {
		t.Fatal("revocation check error must deny")
	}
	p.Revoker = nil
	if d := p.Decide(sin, harness.ToolRequest{Kind: harness.ToolRead}); d.Allow {
		t.Fatal("no revoker must deny")
	}
}

func TestWindowsPaths(t *testing.T) {
	p := Policy{GOOS: "windows", Home: `C:\Users\sin`, Workdir: `C:\Users\sin\work`, StrangerGuard: true, Revoker: revoker{}}
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics need filepath on windows")
	}
	for path, want := range map[string]bool{`src\a.go`: true, `C:\Users\sin\WORK\b.txt`: true, `a.txt:secret`: false, `..\x`: false} {
		if d := p.Decide(stranger, harness.ToolRequest{Kind: harness.ToolRead, Paths: []string{path}}); d.Allow != want {
			t.Fatalf("%s: allow=%v want %v (%s)", path, d.Allow, want, d.Reason)
		}
	}
}
