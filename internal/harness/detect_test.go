package harness_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/RCX1t7/plexus/internal/adapters/all"
	"github.com/RCX1t7/plexus/internal/harness"
)

// winEnv simulates a Windows machine with the given files present and
// PATH entries resolvable.
func winEnv(files map[string]bool, path map[string]string) harness.Env {
	vars := map[string]string{
		"USERPROFILE":  `C:\Users\sin`,
		"LOCALAPPDATA": `C:\Users\sin\AppData\Local`,
		"APPDATA":      `C:\Users\sin\AppData\Roaming`,
	}
	return harness.Env{
		GOOS: "windows", Home: `C:\Users\sin`,
		Getenv: func(k string) string { return vars[k] },
		LookPath: func(n string) (string, error) {
			if p, ok := path[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Exists:     func(p string) bool { return files[p] },
		RunVersion: func(context.Context, string, ...string) (string, error) { return "1.2.3\n", nil },
	}
}

func TestFindExecutableWindowsPrefersExeOverShim(t *testing.T) {
	l := harness.Lookup{Names: []string{"claude"}, Npm: []string{"@anthropic-ai/claude-code/bin/claude.exe"}}
	// claude.cmd comes first on PATH, but claude.exe is asked for explicitly.
	env := winEnv(nil, map[string]string{"claude": `C:\npm\claude.cmd`, "claude.exe": `C:\bin\claude.exe`})
	if got := harness.FindExecutable(env, "", l); got != `C:\bin\claude.exe` {
		t.Fatalf("got %q", got)
	}
}

func TestFindExecutableWindowsKnownDirsAndNpm(t *testing.T) {
	l := harness.Lookup{Names: []string{"claude"}, Npm: []string{"@anthropic-ai/claude-code/bin/claude.exe"}}
	native := filepath.Join(`C:\Users\sin`, ".local", "bin", "claude.exe")
	env := winEnv(map[string]bool{native: true}, map[string]string{"claude": `C:\npm\claude.cmd`})
	if got := harness.FindExecutable(env, "", l); got != native {
		t.Fatalf("known dir: got %q", got)
	}
	npmExe := filepath.Join(`C:\Users\sin\AppData\Roaming`, "npm", "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	env = winEnv(map[string]bool{npmExe: true}, map[string]string{"claude": `C:\npm\claude.cmd`})
	if got := harness.FindExecutable(env, "", l); got != npmExe {
		t.Fatalf("npm native exe: got %q", got)
	}
}

func TestFindExecutableShimOnlyIsLastAndRefused(t *testing.T) {
	l := harness.Lookup{Names: []string{"dsh"}}
	env := winEnv(nil, map[string]string{"dsh": `C:\npm\dsh.cmd`})
	got := harness.FindExecutable(env, "", l)
	if got != `C:\npm\dsh.cmd` || !harness.IsShim(got) {
		t.Fatalf("got %q", got)
	}
	if _, _, err := harness.Command(got, nil); err == nil || !strings.Contains(err.Error(), "shim") {
		t.Fatalf("shim must be refused, err=%v", err)
	}
}

func TestFindExecutableOverride(t *testing.T) {
	env := winEnv(map[string]bool{`D:\tools\codex.exe`: true}, nil)
	if got := harness.FindExecutable(env, `D:\tools\codex.exe`, harness.Lookup{Names: []string{"codex"}}); got != `D:\tools\codex.exe` {
		t.Fatalf("got %q", got)
	}
	if got := harness.FindExecutable(env, `D:\missing.exe`, harness.Lookup{Names: []string{"codex"}}); got != "" {
		t.Fatalf("missing override must not fall back, got %q", got)
	}
}

func TestDetectAllFlagsShims(t *testing.T) {
	env := winEnv(nil, map[string]string{"claude": `C:\npm\claude.cmd`})
	seen := false
	for _, r := range harness.DetectAll(context.Background(), env) {
		if r.Path == `C:\npm\claude.cmd` {
			seen = true
			if r.Error == "" {
				t.Fatalf("shim detection should carry an explanation: %+v", r)
			}
		}
	}
	if !seen {
		t.Fatal("claude shim not reported")
	}
}

func TestChildEnvScrubsClaudeVars(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SIMPLE", "1")
	for _, kv := range harness.ChildEnv([]string{"X=1"}) {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_SIMPLE=") {
			t.Fatalf("not scrubbed: %s", kv)
		}
	}
}
