package dsh

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// manifest reads bridge/plexus-bridge.sha256 (sha256sum format, paths
// relative to bridge/): the artifact first, then every build input.
func manifest(t *testing.T) (artifact string, inputs map[string]string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("bridge", "plexus-bridge.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	inputs = map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for i := 0; sc.Scan(); i++ {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(sum) != 64 {
			t.Fatalf("manifest line %d malformed: %q", i+1, sc.Text())
		}
		if i == 0 {
			if name != pluginFile {
				t.Fatalf("manifest must start with %s, got %q", pluginFile, name)
			}
			artifact = sum
			continue
		}
		inputs[name] = sum
	}
	return artifact, inputs
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// The embedded bridge is exactly the artifact whose hash is recorded.
func TestEmbeddedBridgeMatchesRecordedHash(t *testing.T) {
	want, _ := manifest(t)
	js := pluginJS()
	if len(js) == 0 {
		t.Fatal("bridge artifact not embedded")
	}
	if got := sum(js); got != want {
		t.Fatalf("embedded %s sha256 %s, recorded %s: rebuild with bridge/plugin/scripts/build.mjs", pluginFile, got, want)
	}
}

// Only the artifact is embedded: no TypeScript, tests or node_modules.
func TestEmbedHoldsOnlyTheArtifact(t *testing.T) {
	var files []string
	_ = fs.WalkDir(bridgeFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if len(files) != 1 || files[0] != "bridge/"+pluginFile {
		t.Fatalf("embedded files: %v", files)
	}
}

// Source and artifact are in sync without running node: build.mjs records the
// hash of every build input next to the artifact's, so editing a source file
// (or adding one) without rebuilding fails here. The rebuild itself is
// verified by `node scripts/build.mjs --check` (CI).
func TestBridgeSourcesMatchManifest(t *testing.T) {
	_, inputs := manifest(t)
	for _, must := range []string{"plugin/package.json", "plugin/package-lock.json", "plugin/scripts/build.mjs"} {
		if _, ok := inputs[must]; !ok {
			t.Errorf("manifest does not list %s", must)
		}
	}
	srcs, _ := filepath.Glob(filepath.Join("bridge", "plugin", "src", "*.ts"))
	if len(srcs) == 0 {
		t.Fatal("no plugin sources in bridge/plugin/src")
	}
	for _, p := range srcs {
		rel := filepath.ToSlash(strings.TrimPrefix(p, "bridge"+string(filepath.Separator)))
		if _, ok := inputs[rel]; !ok {
			t.Errorf("%s is not in the manifest: rebuild", rel)
		}
	}
	names := make([]string, 0, len(inputs))
	for n := range inputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("bridge", filepath.FromSlash(n)))
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if got := sum(b); got != inputs[n] {
			t.Errorf("%s changed since the last build (sha256 %s, recorded %s): run `node scripts/build.mjs` in bridge/plugin", n, got, inputs[n])
		}
	}
}
