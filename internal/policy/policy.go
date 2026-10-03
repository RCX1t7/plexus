// Package policy makes every tool permission decision. Adapters only
// translate native requests into harness.ToolRequest; the rules live here.
//
// The model is trust, not permission levels: Sin and all Plexus partners
// trust each other fully. Exactly one boundary exists, on by default (the
// "stranger guard"): a turn started by a Slack user who is neither Sin nor
// a partner may get conversation and help, and may let the partner read
// ordinary files in its workdir, but cannot make it run commands, write
// files or fetch from the network. Stop is a control feature, not
// authorization: a stopped task tree gets no further tool calls.
package policy

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/platform"
)

// Source says who started a turn.
type Source string

const (
	FromSin      Source = "sin"      // a configured owner (Sin)
	FromPartner  Source = "partner"  // another Plexus partner, on a trusted request
	FromStranger Source = "stranger" // anyone else, or a partner relaying a stranger's request
)

// Authority is who a turn acts for, and the task tree it belongs to.
type Authority struct {
	Source Source
	Root   string // task tree id ("" = none); revoked by stop
}

// Revoker reports whether a task tree was stopped.
type Revoker interface {
	AnyRevoked(ids ...string) (bool, error)
}

// Policy holds the machine-specific inputs to decisions.
type Policy struct {
	GOOS          string
	Home          string
	Workdir       string
	ExtraDeny     []string // extra paths strangers may not read (absolute or workdir-relative)
	StrangerGuard bool
	Revoker       Revoker
}

// Restricted reports whether a turn from this source is under the guard.
func (p Policy) Restricted(a Authority) bool { return p.StrangerGuard && a.Source == FromStranger }

// Level is the native sandbox hint for a turn (defense in depth).
func (p Policy) Level(a Authority) harness.Level {
	if p.Restricted(a) {
		return harness.LevelChat // strangers: conversation, never write or execute
	}
	return harness.LevelFull
}

// Decide answers one tool request. A stop is re-checked first, so a
// stopped tree cannot run a new tool even if an interrupt was missed.
func (p Policy) Decide(a Authority, req harness.ToolRequest) harness.Decision {
	deny := func(r string) harness.Decision { return harness.Decision{Allow: false, Reason: r} }
	allow := func(r string) harness.Decision { return harness.Decision{Allow: true, Reason: r} }
	if a.Root != "" {
		if p.Revoker == nil {
			return deny("stop list unavailable")
		}
		if stopped, err := p.Revoker.AnyRevoked(a.Root); err != nil || stopped {
			return deny("this task was stopped")
		}
	}
	switch req.Kind {
	case harness.ToolMeta, harness.ToolAsk:
		return allow("no side effects")
	}
	if !p.Restricted(a) {
		return allow("trusted")
	}
	if req.Kind != harness.ToolRead {
		return deny("requests from people outside the team cannot run commands, write files or use the network")
	}
	paths := req.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	for _, path := range paths {
		if ok, why := p.readable(path); !ok {
			return deny(why)
		}
	}
	return allow("read inside workdir")
}

// resolve makes path absolute (relative to the workdir), cleans it and
// follows symlinks of the existing prefix, so links cannot escape.
func (p Policy) resolve(path string) string {
	if p.GOOS == "windows" {
		path = strings.ReplaceAll(path, "/", `\`)
	}
	if !filepath.IsAbs(path) && !(p.GOOS == "windows" && len(path) > 1 && path[1] == ':') {
		path = filepath.Join(p.Workdir, path)
	}
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	} else if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	return platform.NormPath(p.GOOS, path)
}

func (p Policy) within(path, root string) bool {
	root = p.resolve(root)
	sep := string(os.PathSeparator)
	if p.GOOS == "windows" {
		sep = `\`
	}
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, sep)+sep)
}

// secretDirs are credential stores and harness homes, relative to Home.
var secretDirs = []string{".claude", ".codex", ".ssh", ".aws", ".azure", ".gnupg", ".docker", ".kube",
	".config/gh", ".config/gcloud", ".gemini", ".dsh", ".deepseek",
	"AppData/Roaming/Microsoft/Credentials", "AppData/Local/Microsoft/Credentials",
	"AppData/Roaming/Microsoft/Protect", "AppData/Roaming/Microsoft/Crypto"}

// secretNames are file names strangers can never have read.
var secretNames = []string{".git-credentials", ".netrc", "_netrc", ".npmrc", ".pypirc", ".pgpass",
	"credentials.json", ".credentials.json", "auth.json", "id_rsa", "id_ed25519", "id_ecdsa"}

var secretExts = []string{".pem", ".key", ".pfx", ".p12", ".kdbx"}

// dataDirNames are directory names inside the workdir treated as private data.
var dataDirNames = []string{"data", ".data", "secrets", ".secrets", ".plexus"}

func (p Policy) readable(path string) (bool, string) {
	r := p.resolve(path)
	if p.GOOS == "windows" && strings.Contains(r[min(2, len(r)):], ":") {
		return false, "alternate data streams are not allowed"
	}
	if !p.within(r, p.Workdir) {
		return false, "outside the workdir"
	}
	for _, d := range secretDirs {
		if p.Home != "" && p.within(r, filepath.Join(p.Home, filepath.FromSlash(d))) {
			return false, "credential store"
		}
	}
	for _, d := range p.ExtraDeny {
		if p.within(r, d) {
			return false, "denied by config"
		}
	}
	rel := strings.TrimPrefix(r, p.resolve(p.Workdir))
	for _, seg := range strings.FieldsFunc(rel, func(c rune) bool { return c == '/' || c == '\\' }) {
		for _, n := range dataDirNames {
			if seg == n {
				return false, "private data dir"
			}
		}
	}
	base := strings.ToLower(filepath.Base(r))
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return false, "environment file"
	}
	for _, n := range secretNames {
		if base == n || strings.HasPrefix(base, n+".") {
			return false, "credential file"
		}
	}
	for _, e := range secretExts {
		if strings.HasSuffix(base, e) {
			return false, "key file"
		}
	}
	return true, ""
}
