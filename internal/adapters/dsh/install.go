package dsh

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/RCX1t7/plexus/internal/harness"
)

// The bridge plugin: bridge/plexus-bridge.min.mjs, built from the TypeScript
// source in bridge/plugin (see bridge/README.md). Only the artifact is
// embedded, never the source, tests or node_modules; its sha256 is recorded
// in bridge/plexus-bridge.sha256 and checked by bridge_test.go.
//
//go:embed bridge/plexus-bridge.min.mjs
var bridgeFS embed.FS

// ProfileName is the DSH profile Plexus runs (never DSH Desktop's "desktop").
const ProfileName = "plexus"

// BridgeVersion stamps the profile package.json and the overlay. It is kept
// in sync with the TypeScript build (protocol.ts BRIDGE_VERSION, injected by
// bridge/plugin/scripts/build.mjs). A non-empty version is required: DSH's
// plugin-package-inventory throws on a profile package.json without one
// (REQUEST_EXTENSION on every model request otherwise).
const BridgeVersion = "1.0.0"

const (
	pluginFile  = "plexus-bridge.min.mjs"
	stampFile   = "plexus-bridge.json"
	overlayFile = "plexus.patch.yml"
)

func pluginJS() []byte {
	b, _ := bridgeFS.ReadFile("bridge/" + pluginFile)
	return b
}

// Bundled reports whether this build carries the bridge plugin.
func Bundled() bool { return len(pluginJS()) > 0 }

// overlay is the Plexus-owned layer passed with --patch (loaded after the
// profile's and the home cordis.patch.yml, so these rows win). It
//   - swaps the SDK JSON-RPC server row for the bridge plugin (same inject
//     contract: DSH's sdk-app owns the stdio lifecycle);
//   - pins the permission defaults so no environment variable or default
//     preset starts a Plexus session in an auto-approve mode: sandbox
//     workspace-write, approval policy "ask";
//   - disables the data-egress rows (session log to DeepSeek, OTEL telemetry,
//     the package inventory) so no transcript, plugin list or telemetry leaves
//     the machine by default (CR-10). Each is opt-in again via the user's own
//     cordis.patch.yml;
//   - disables tool-plugin-manager (CR-10): the agent cannot install packages.
//     Tagging it sys.installer in the single Go classifier is a skeleton-side
//     request (internal/danger), out of this patch's scope;
//   - adds DSH's own ask_user_question tool.
//
// Row ids and config keys mirror packages/bundle/base and
// packages/bundle/sdk-app (@ 0.2.0-rc.2). A row patch replaces that row's
// whole config, so every key the base row sets is repeated.
const overlay = `# Plexus-owned overlay for "dsh --profile plexus" (written by plexus setup;
# regenerated on upgrade, do not edit; put your own rows in cordis.patch.yml).

- id: sdk-app-startup
  config:
    profile: plexus

- id: sdk-jsonrpc-server
  disabled: true

# No auto-approve: fixed workspace-write sandbox and "ask" approvals,
# regardless of any environment override. Dangerous actions are additionally
# gated by the bridge (tools/pre-execute + tools.guard) forwarding every
# non-read call to Plexus, whose single Go classifier decides.
- id: sandbox-policy
  config:
    mode: workspace-write
    workspaceRoot: !!js process.cwd()

- id: approval
  config:
    policy: ask

- id: permission
  config:
    defaultPreset: workspace-write
    presets:
      read-only:
        sandbox: read-only
        approval: ask
      workspace-write:
        sandbox: workspace-write
        approval: ask
      # Even the full-access preset asks (CR-10): no preset may auto-approve.
      # The sandbox can still be widened for a turn, but every non-read call is
      # gated by the bridge + the single Go classifier regardless of preset.
      danger-full-access:
        sandbox: danger-full-access
        approval: ask

# Privacy (CR-10): nothing about a Plexus turn leaves the machine by default.
# Re-enable any of these in your own cordis.patch.yml if you want them.
- id: session-log-deepseek
  disabled: true
- id: session-telemetry-otel
  disabled: true
- id: plugin-package-inventory-deepseek
  disabled: true
- id: tool-plugin-manager
  disabled: true

- insert:
    - id: plexus-bridge
      name: './` + pluginFile + `'
      inject: [sdkAppStartup, agents, loader]

    - id: plexus-tool-ask-user
      name: '@deepseek-ai/dsh-tool-ask-user'
`

// profilePatchTemplate and workspaceTemplate mirror DSH's initProfile
// (packages/boot/app-boot/src/profile.ts); written only if absent.
const (
	profilePatchTemplate = "# Your patch layer for this dsh profile, applied after every bundle layer:\n" +
		"# a top-level YAML array of loader patch entries (id-targeted config\n" +
		"# overrides, disables, and insert lists; `!!js` expressions allowed).\n[]\n"
	workspaceTemplate = "packages:\n  - .\n\nnodeLinker: hoisted\nautoInstallPeers: false\n"
)

var profileBundles = []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-sdk-app"}

// InstallResult reports what Install wrote.
type InstallResult struct {
	ProfileDir string
	PluginPath string
	PatchPath  string   // the --patch overlay
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

// InstallAt writes the plexus profile into home/profiles/plexus. It is
// idempotent: files are rewritten (atomically) only when their content
// differs; the user-owned cordis.patch.yml and pnpm-workspace.yaml are created
// once and never touched again; package.json is merged (name and version are
// filled when missing). It never touches other profiles (DSH Desktop owns
// profiles/desktop).
func InstallAt(home string) (InstallResult, error) {
	js := pluginJS()
	dir := filepath.Join(home, "profiles", ProfileName)
	sum := sha256.Sum256(js)
	r := InstallResult{ProfileDir: dir, PluginPath: filepath.Join(dir, pluginFile),
		PatchPath: filepath.Join(dir, overlayFile), SHA256: hex.EncodeToString(sum[:])}
	if len(js) == 0 {
		return r, errors.New("this build does not include the DSH bridge plugin yet")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, err
	}
	pkg, err := mergedManifest(filepath.Join(dir, "package.json"))
	if err != nil {
		return r, err
	}
	stamp, _ := json.MarshalIndent(map[string]any{"name": "plexus-dsh-bridge", "version": BridgeVersion,
		"protocol": Protocol, "sha256": r.SHA256, "file": pluginFile}, "", "  ")
	files := []struct {
		name   string
		data   []byte
		create bool // create-only (user-owned afterwards)
	}{
		{"package.json", pkg, false},
		{"cordis.patch.yml", []byte(profilePatchTemplate), true},
		{"pnpm-workspace.yaml", []byte(workspaceTemplate), true},
		{pluginFile, js, false},
		{overlayFile, []byte(overlay), false},
		{stampFile, append(stamp, '\n'), false},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		old, err := os.ReadFile(p)
		switch {
		case err == nil && (f.create || bytes.Equal(old, f.data)):
			continue
		case err != nil && !os.IsNotExist(err):
			return r, err
		}
		if err := writeAtomic(p, f.data); err != nil {
			return r, err
		}
		r.Changed = append(r.Changed, f.name)
	}
	return r, nil
}

// Installed reports whether the profile at home carries this build's plugin.
func Installed(home string) bool {
	js := pluginJS()
	b, err := os.ReadFile(filepath.Join(home, "profiles", ProfileName, pluginFile))
	return len(js) > 0 && err == nil && bytes.Equal(b, js)
}

// mergedManifest returns package.json carrying a name, a non-empty version and
// dsh.profile.bundles with the Plexus bundles (in order, first), keeping every
// other field. It returns the existing bytes unchanged when nothing needs
// adding. A missing name or version is filled: DSH's package inventory throws
// on a profile manifest without a version (CR-2).
func mergedManifest(path string) ([]byte, error) {
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b, _ := json.MarshalIndent(map[string]any{
			"name": "dsh-profile-" + ProfileName, "version": BridgeVersion, "private": true,
			"dependencies": map[string]any{},
			"dsh":          map[string]any{"profile": map[string]any{"bundles": profileBundles}},
		}, "", "  ")
		return append(b, '\n'), nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(old, &m); err != nil || m == nil {
		return nil, fmt.Errorf("%s is not a JSON object; fix or remove it", path)
	}
	changed := false
	if s, _ := m["name"].(string); strings.TrimSpace(s) == "" {
		m["name"] = "dsh-profile-" + ProfileName
		changed = true
	}
	if s, _ := m["version"].(string); strings.TrimSpace(s) == "" {
		m["version"] = BridgeVersion
		changed = true
	}
	d, _ := m["dsh"].(map[string]any)
	if d == nil {
		d = map[string]any{}
	}
	p, _ := d["profile"].(map[string]any)
	if p == nil {
		p = map[string]any{}
	}
	var have []string
	if arr, ok := p["bundles"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				have = append(have, s)
			}
		}
	}
	want := append([]string(nil), profileBundles...)
	for _, s := range have {
		if !contains(profileBundles, s) {
			want = append(want, s)
		}
	}
	if !equal(have, want) {
		p["bundles"] = want
		d["profile"] = p
		m["dsh"] = d
		changed = true
	}
	if !changed {
		return old, nil
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n'), nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plexus-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path) // replaces on Windows too (MoveFileEx)
		if werr != nil && runtime.GOOS == "windows" {
			werr = fmt.Errorf("replace %s: %w", filepath.Base(path), werr)
		}
	}
	if werr != nil {
		os.Remove(name)
	}
	return werr
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
