package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/RCX1t7/plexus/internal/platform"
)

// Env is the slice of the OS that detection looks at. Tests replace it to
// simulate Windows on Linux.
type Env struct {
	GOOS     string
	Home     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	Exists   func(string) bool
	// RunVersion runs exe with args and returns its stdout. Only ever called
	// with --version / --help style arguments.
	RunVersion func(ctx context.Context, exe string, args ...string) (string, error)
}

// OSEnv returns the real environment.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{
		GOOS:     runtime.GOOS,
		Home:     home,
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		RunVersion: runVersion,
	}
}

func runVersion(ctx context.Context, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, argv, err := Command(exe, args)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	platform.HideWindow(cmd)
	cmd.Env = ChildEnv(nil)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// scrubbed are inherited variables that silently change a harness: e.g.
// CLAUDE_CODE_SIMPLE=1 turns on Claude Code bare mode (no skills, MCP,
// hooks, subagents), CLAUDECODE makes it think it runs inside a parent.
var scrubbed = []string{"CLAUDECODE", "CLAUDE_CODE_SIMPLE"}

// ChildEnv is os.Environ() minus scrubbed variables, plus extra.
func ChildEnv(extra []string) []string {
	var out []string
	for _, kv := range os.Environ() {
		drop := false
		for _, k := range scrubbed {
			if strings.HasPrefix(strings.ToUpper(kv), k+"=") {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// Command maps a resolved program to what is actually spawned: a .js entry
// point runs under node; Windows batch files (npm .cmd shims) are refused,
// because cmd.exe re-parses arguments (BatBadBut) and they break tree kill.
func Command(exe string, args []string) (string, []string, error) {
	switch strings.ToLower(filepath.Ext(exe)) {
	case ".cmd", ".bat":
		return "", nil, fmt.Errorf("%s is an npm/batch shim; install the native build or set the exe path to the real .exe", exe)
	case ".js", ".mjs", ".cjs":
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, fmt.Errorf("%s needs node on PATH: %w", exe, err)
		}
		return node, append([]string{exe}, args...), nil
	}
	return exe, args, nil
}

// Lookup says how to find one CLI.
type Lookup struct {
	Names []string // command names without extension, e.g. "claude"
	// Npm lists entry points inside the npm global node_modules dir, best
	// first: a native .exe shipped in the package, or a .js run with node.
	// They are preferred over the package's .cmd shim.
	Npm []string
}

// Candidates lists well-known install locations for a CLI named name.
// On Windows that is the native installer dir, scoop and WinGet links
// (.exe only); elsewhere ~/.local/bin and npm-global.
func Candidates(env Env, name string) []string {
	if env.GOOS == "windows" {
		profile := firstNonEmpty(env.Getenv("USERPROFILE"), env.Home)
		local := env.Getenv("LOCALAPPDATA")
		var c []string
		exe := name + ".exe"
		if profile != "" {
			c = append(c, filepath.Join(profile, ".local", "bin", exe), filepath.Join(profile, "scoop", "shims", exe))
		}
		if local != "" {
			c = append(c, filepath.Join(local, "Microsoft", "WinGet", "Links", exe),
				filepath.Join(local, "Programs", name, exe))
		}
		return c
	}
	if env.Home == "" {
		return nil
	}
	return []string{
		filepath.Join(env.Home, ".local", "bin", name),
		filepath.Join(env.Home, ".npm-global", "bin", name),
		filepath.Join(env.Home, "bin", name),
	}
}

// FindExecutable resolves, in order: an explicit override; on Windows the
// real name.exe on PATH (asked for explicitly, so an earlier name.cmd does
// not win) and in known install dirs; the native exe or .js inside the npm
// package; and only last any shim on PATH (which StartProc then refuses
// with an explanation).
func FindExecutable(env Env, override string, l Lookup) string {
	if override != "" {
		if env.Exists(override) {
			return override
		}
		if p, err := env.LookPath(override); err == nil {
			return p
		}
		return ""
	}
	win := env.GOOS == "windows"
	for _, n := range l.Names {
		q := n
		if win {
			q = n + ".exe"
		}
		if p, err := env.LookPath(q); err == nil && p != "" {
			return p
		}
	}
	for _, n := range l.Names {
		for _, c := range Candidates(env, n) {
			if env.Exists(c) {
				return c
			}
		}
	}
	if root := npmRoot(env); root != "" {
		for _, rel := range l.Npm {
			if c := filepath.Join(root, filepath.FromSlash(rel)); env.Exists(c) {
				return c
			}
		}
	}
	if win {
		for _, n := range l.Names {
			if p, err := env.LookPath(n); err == nil && p != "" {
				return p // a .cmd shim: reported, refused at spawn
			}
			if appdata := env.Getenv("APPDATA"); appdata != "" {
				if c := filepath.Join(appdata, "npm", n+".cmd"); env.Exists(c) {
					return c
				}
			}
		}
	}
	return ""
}

func npmRoot(env Env) string {
	if env.GOOS == "windows" {
		if a := env.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "npm", "node_modules")
		}
		return ""
	}
	if env.Home == "" {
		return ""
	}
	return filepath.Join(env.Home, ".npm-global", "lib", "node_modules")
}

// IsShim reports whether path is a batch shim that cannot be spawned.
func IsShim(path string) bool {
	e := strings.ToLower(filepath.Ext(path))
	return e == ".cmd" || e == ".bat"
}

// FirstLine returns the first non-empty line of s, trimmed and capped.
func FirstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 120 {
				l = l[:120]
			}
			return l
		}
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
