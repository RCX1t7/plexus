package dsh

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

func env(node string, files map[string]bool, path map[string]string) harness.Env {
	return harness.Env{GOOS: "linux", Home: "/home/u",
		Getenv: func(k string) string {
			switch k {
			case "APPDATA":
				return `C:\Users\u\AppData\Roaming`
			case "USERPROFILE":
				return `C:\Users\u`
			}
			return ""
		},
		LookPath: func(n string) (string, error) {
			if p, ok := path[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Exists: func(p string) bool { return files[filepath.Clean(p)] },
		RunVersion: func(_ context.Context, exe string, args ...string) (string, error) {
			if len(args) == 1 && args[0] == "--version" {
				return node + "\n", nil
			}
			return "dsh 0.2.0\n", nil
		}}
}

func TestDetectNodeAndShims(t *testing.T) {
	js := "/home/u/.npm-global/lib/node_modules/@deepseek-ai/dsh/lib/bin.js"
	node := map[string]string{"node": "/usr/bin/node"}
	ctx := context.Background()

	r := Adapter{}.Detect(ctx, env("v22.19.0", map[string]bool{js: true}, node))
	if r.Path != js || r.Version != "dsh 0.2.0" || strings.Contains(r.Error, "Node") {
		t.Fatalf("%+v", r)
	}
	r = Adapter{}.Detect(ctx, env("v22.18.1", map[string]bool{js: true}, node))
	if !strings.Contains(r.Error, "too old") || r.Version != "" {
		t.Fatalf("old node not reported: %+v", r)
	}
	r = Adapter{}.Detect(ctx, env("v24.1.0", map[string]bool{js: true}, nil))
	if !strings.Contains(r.Error, "Node.js not found") {
		t.Fatalf("missing node not reported: %+v", r)
	}
	// only DSH Desktop's dsh.cmd on PATH: never used
	desk := map[string]string{"dsh": "/opt/DSH Desktop/resources/bin/dsh.cmd"}
	r = Adapter{}.Detect(ctx, env("v24.1.0", nil, desk))
	if r.Installed || r.Path != "" || !strings.Contains(r.Error, "never runs") {
		t.Fatalf("desktop shim accepted: %+v", r)
	}
}

func TestParseNode(t *testing.T) {
	for v, want := range map[string][2]int{"v22.19.0": {22, 19}, "v24.0.1": {24, 0}, "22.3": {22, 3}} {
		if a, b, ok := parseNode(v); !ok || a != want[0] || b != want[1] {
			t.Fatal(v, a, b, ok)
		}
	}
	if _, _, ok := parseNode("node"); ok {
		t.Fatal("garbage parsed")
	}
}

func TestInstallSlot(t *testing.T) {
	home := t.TempDir()
	if Bundled() {
		t.Skip("plugin bundled: covered by the plugin's own install tests")
	}
	if _, err := InstallAt(home); err == nil || Installed(home) {
		t.Fatal("installed without a bundled plugin")
	}
	if DSHHome(harness.Env{GOOS: "linux", Home: "/h", Getenv: func(k string) string {
		if k == "DSH_HOME" {
			return "~/d"
		}
		return ""
	}}) != filepath.Join("/h", "d") {
		t.Fatal("DSH_HOME ~ not expanded")
	}
}
