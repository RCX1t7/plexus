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
		if p.Guest(a) {
			t.Fatal("trusted level must be full")
		}
	}
}

func TestStrangerGetsNoToolsButPost(t *testing.T) {
	p, wd := setup(t)
	for _, k := range []harness.ToolKind{harness.ToolRead, harness.ToolShell, harness.ToolWrite, harness.ToolFetch,
		harness.ToolOther, harness.ToolMeta, harness.ToolAsk} {
		if d := p.Decide(stranger, harness.ToolRequest{Name: "x", Kind: k, Paths: []string{filepath.Join(wd, "main.go")}}); d.Allow {
			t.Fatalf("stranger %s allowed", k)
		}
	}
	for _, n := range []string{"plexus_post", "mcp__plexus__plexus_post"} {
		if d := p.Decide(stranger, harness.ToolRequest{Name: n, Kind: harness.ToolMeta}); !d.Allow {
			t.Fatalf("%s denied: %s", n, d.Reason)
		}
	}
	for _, n := range []string{"mcp__plexus__plexus_delegate", "plexus_postx", "Read"} {
		if d := p.Decide(stranger, harness.ToolRequest{Name: n, Kind: harness.ToolMeta}); d.Allow {
			t.Fatalf("%s allowed", n)
		}
	}
	if !p.Guest(stranger) {
		t.Fatal("stranger level must be chat")
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
