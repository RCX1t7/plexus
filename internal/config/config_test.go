package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

func TestLoadSaveDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir) // missing file: defaults
	if err != nil {
		t.Fatal(err)
	}
	if !c.Guard() || c.SetupPort == 0 {
		t.Fatalf("defaults: guard=%v port=%d", c.Guard(), c.SetupPort)
	}
	c.Owners = []string{"U0SIN1234"}
	c.SetGuard(false)
	added := c.AutoAdd([]harness.DetectionResult{{Harness: "claude_code", Installed: true}}, dir)
	if len(added) != 1 {
		t.Fatalf("autoadd %v", added)
	}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	// Unix: 0600. Windows has no mode bits (os.Stat reports 0666 for any
	// writable file); there config.json relies on the per-user profile ACL
	// of its directory, and it holds no secrets (tokens live in DPAPI).
	if fi, _ := os.Stat(Path(dir)); runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config.json too open: %v", fi.Mode())
	}
	c2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Guard() || len(c2.Owners) != 1 || len(c2.Bots) != 1 || c2.Bots[0].Workdir == "" {
		t.Fatalf("roundtrip %+v", c2)
	}
	c2.Owners = []string{"sin"}
	if c2.Validate() == nil {
		t.Fatal("bad owner id accepted")
	}
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"owners":["nope"]}`), 0o600)
	if _, err := Load(dir); err == nil {
		t.Fatal("invalid config loaded")
	}
}
