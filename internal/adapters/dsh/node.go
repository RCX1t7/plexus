package dsh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RCX1t7/plexus/internal/harness"
)

// MinNode is the oldest Node.js DSH runs on.
var MinNode = [2]int{22, 19}

// find resolves DSH's JS entry point (or a native dsh on non-Windows PATH).
// It never returns a .cmd/.bat shim, so DSH Desktop's dsh.cmd is never used;
// the second result explains a shim that was found and skipped.
func find(env harness.Env) (string, string) {
	if p := harness.FindExecutable(env, env.Getenv("PLEXUS_DSH_EXE"), lookup); p != "" {
		if !harness.IsShim(p) && !desktopPath(p) {
			return p, ""
		}
		return "", "only " + p + " was found; Plexus never runs DSH Desktop's or npm's dsh.cmd. " +
			"Install the CLI with `npm i -g @deepseek-ai/dsh` (or set PLEXUS_DSH_EXE to its lib/bin.js)"
	}
	return "", ""
}

// desktopPath reports whether p sits inside DSH Desktop's install tree.
func desktopPath(p string) bool {
	l := strings.ToLower(filepath.ToSlash(p))
	return strings.Contains(l, "/dsh desktop/") || strings.Contains(l, "/dsh-desktop/") || strings.Contains(l, "/deepseek harness/")
}

func isJS(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".js", ".mjs", ".cjs":
		return true
	}
	return false
}

// findNode prefers node next to npm's prefix (npm's own rule), then PATH.
func findNode(env harness.Env, script string) (string, error) {
	name := "node"
	if env.GOOS == "windows" {
		name = "node.exe"
	}
	// <prefix>/node_modules/@deepseek-ai/dsh/lib/bin.js -> <prefix>/node(.exe)
	dir := filepath.Dir(script)
	for i := 0; i < 5; i++ {
		dir = filepath.Dir(dir)
	}
	for _, c := range []string{filepath.Join(dir, name), filepath.Join(filepath.Dir(dir), "bin", name)} {
		if env.Exists(c) {
			return c, nil
		}
	}
	if p, err := env.LookPath(name); err == nil && p != "" {
		return p, nil
	}
	return "", fmt.Errorf("Node.js not found; DSH needs Node.js >= %d.%d", MinNode[0], MinNode[1])
}

// checkNode runs `node --version` and reports an error when it is older than MinNode.
func checkNode(ctx context.Context, env harness.Env, node string) (string, error) {
	out, err := env.RunVersion(ctx, node, "--version")
	if err != nil {
		return "", errors.New("could not run `node --version`")
	}
	v := harness.FirstLine(out)
	maj, min, ok := parseNode(v)
	if !ok {
		return v, fmt.Errorf("unrecognised Node.js version %q; DSH needs Node.js >= %d.%d", v, MinNode[0], MinNode[1])
	}
	if maj < MinNode[0] || (maj == MinNode[0] && min < MinNode[1]) {
		return v, fmt.Errorf("Node.js %s is too old; DSH needs Node.js >= %d.%d", v, MinNode[0], MinNode[1])
	}
	return v, nil
}

func parseNode(v string) (int, int, bool) {
	f := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(f) < 2 {
		return 0, 0, false
	}
	maj, e1 := strconv.Atoi(f[0])
	min, e2 := strconv.Atoi(f[1])
	return maj, min, e1 == nil && e2 == nil
}
