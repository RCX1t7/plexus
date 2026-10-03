package dsh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

// InstallAt must write a complete, self-contained plexus profile: the bundled
// plugin, the overlay that pins approvals and turns off every data-egress row
// (CR-10), and a package.json carrying a name and a non-empty version (CR-2).
func TestInstallAtWritesVersionedProfileAndPrivacyPins(t *testing.T) {
	if !Bundled() {
		t.Skip("plugin not bundled in this build")
	}
	home := t.TempDir()
	r, err := InstallAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if !Installed(home) {
		t.Fatal("Installed reports false right after InstallAt")
	}
	for _, name := range []string{pluginFile, overlayFile, "package.json", "cordis.patch.yml", "pnpm-workspace.yaml", stampFile} {
		if _, err := os.Stat(filepath.Join(r.ProfileDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	// package.json: name and version both non-empty (CR-2).
	var pkg map[string]any
	readJSON(t, filepath.Join(r.ProfileDir, "package.json"), &pkg)
	if s, _ := pkg["name"].(string); strings.TrimSpace(s) == "" {
		t.Error("package.json has no name")
	}
	if s, _ := pkg["version"].(string); strings.TrimSpace(s) == "" {
		t.Error("package.json has no version")
	}

	// Overlay: approvals pinned, every egress row disabled, plugin inserted (CR-10).
	ov := readFile(t, r.PatchPath)
	for _, want := range []string{
		"id: sdk-jsonrpc-server", "id: plexus-bridge", "id: plexus-tool-ask-user",
		"policy: ask", "id: session-log-deepseek", "id: session-telemetry-otel",
		"id: plugin-package-inventory-deepseek", "id: tool-plugin-manager",
	} {
		if !strings.Contains(ov, want) {
			t.Errorf("overlay missing %q", want)
		}
	}
	// Each egress / installer row is actually disabled.
	for _, id := range []string{"session-log-deepseek", "session-telemetry-otel", "plugin-package-inventory-deepseek", "tool-plugin-manager", "sdk-jsonrpc-server"} {
		if !disabledInOverlay(ov, id) {
			t.Errorf("row %q is not disabled in the overlay", id)
		}
	}

	// Idempotent: a second call rewrites nothing.
	r2, err := InstallAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Changed) != 0 {
		t.Errorf("second InstallAt rewrote %v", r2.Changed)
	}
}

// A pre-existing profile package.json without a version is repaired (CR-2):
// DSH's package inventory throws on a profile manifest with no version.
func TestMergedManifestFillsMissingVersion(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "profiles", ProfileName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"mine"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := mergedManifest(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if s, _ := m["version"].(string); strings.TrimSpace(s) == "" {
		t.Fatalf("version not filled: %s", b)
	}
	if m["name"] != "mine" {
		t.Errorf("existing name clobbered: %v", m["name"])
	}
}

func TestDSHHomeTildeExpanded(t *testing.T) {
	got := DSHHome(harness.Env{GOOS: "linux", Home: "/h", Getenv: func(k string) string {
		if k == "DSH_HOME" {
			return "~/d"
		}
		return ""
	}})
	if got != filepath.Join("/h", "d") {
		t.Fatalf("~ not expanded: %q", got)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(readFile(t, p)), v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

// disabledInOverlay reports whether the row id is followed (before the next
// row) by `disabled: true` in the YAML overlay.
func disabledInOverlay(overlay, id string) bool {
	i := strings.Index(overlay, "id: "+id+"\n")
	if i < 0 {
		return false
	}
	rest := overlay[i:]
	if j := strings.Index(rest[1:], "\n- "); j >= 0 {
		rest = rest[:j+1]
	}
	return strings.Contains(rest, "disabled: true")
}
