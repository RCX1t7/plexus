package dsh

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/RCX1t7/plexus/internal/harness"
)

// The bridge plugin slot: bridge/plexus-bridge.min.mjs, bundled by go:embed
// (see bridge/README.md). Until the plugin is dropped in, only the README is
// there and Bundled reports false.
//
//go:embed all:bridge
var bridgeFS embed.FS

// ProfileName is the DSH profile Plexus runs (never DSH Desktop's "desktop").
const ProfileName = "plexus"

const (
	pluginFile  = "plexus-bridge.min.mjs"
	overlayFile = "plexus.patch.yml"
)

func pluginJS() []byte {
	b, _ := bridgeFS.ReadFile("bridge/" + pluginFile)
	return b
}

// Bundled reports whether this build carries the bridge plugin.
func Bundled() bool { return len(pluginJS()) > 0 }

// InstallResult reports what Install wrote.
type InstallResult struct {
	ProfileDir string
	PluginPath string
	PatchPath  string   // the --patch overlay, if the plugin ships one
	Changed    []string // file names rewritten by this call (empty = already current)
	SHA256     string   // of the embedded plugin
}

// DSHHome mirrors DSH: $DSH_HOME (~ expanded), else <home>/.dsh, where home
// is USERPROFILE on Windows.
func DSHHome(env harness.Env) string {
	home := env.Home
	if env.GOOS == "windows" {
		if p := env.Getenv("USERPROFILE"); p != "" {
			home = p
		}
	}
	if h := strings.TrimSpace(env.Getenv("DSH_HOME")); h != "" {
		if h == "~" || strings.HasPrefix(h, "~/") || strings.HasPrefix(h, `~\`) {
			h = filepath.Join(home, h[1:])
		}
		return h
	}
	return filepath.Join(home, ".dsh")
}

// Install writes the plexus profile into DSHHome(env)/profiles/plexus.
func Install(env harness.Env) (InstallResult, error) { return InstallAt(DSHHome(env)) }

// InstallAt writes the bundled plugin into home/profiles/plexus,
// atomically and only when it changed. It never touches other profiles
// (DSH Desktop owns profiles/desktop).
func InstallAt(home string) (InstallResult, error) {
	js := pluginJS()
	dir := filepath.Join(home, "profiles", ProfileName)
	sum := sha256.Sum256(js)
	r := InstallResult{ProfileDir: dir, PluginPath: filepath.Join(dir, pluginFile),
		PatchPath: filepath.Join(dir, overlayFile), SHA256: hex.EncodeToString(sum[:])}
	if len(js) == 0 {
		return r, errors.New("this build does not include the DSH bridge plugin yet")
	}
	if old, err := os.ReadFile(r.PluginPath); err == nil && bytes.Equal(old, js) {
		return r, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, err
	}
	tmp := r.PluginPath + ".tmp"
	if err := os.WriteFile(tmp, js, 0o644); err != nil {
		return r, err
	}
	if err := os.Rename(tmp, r.PluginPath); err != nil {
		return r, err
	}
	r.Changed = append(r.Changed, pluginFile)
	return r, nil
}

// Installed reports whether the profile at home carries this build's plugin.
func Installed(home string) bool {
	js := pluginJS()
	b, err := os.ReadFile(filepath.Join(home, "profiles", ProfileName, pluginFile))
	return len(js) > 0 && err == nil && bytes.Equal(b, js)
}
