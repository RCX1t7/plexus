package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlaceholderOnlyAndInstall(t *testing.T) {
	if Bundled() {
		t.Fatal("this tree ships only the placeholder; update the test when the plugin lands")
	}
	home := t.TempDir()
	dir, err := Install(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil || dir != filepath.Join(home, "plugins", Dir) {
		t.Fatal(dir, err)
	}
}
