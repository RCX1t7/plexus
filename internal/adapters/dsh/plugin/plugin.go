// Package plugin carries the DSH bridge plugin inside plexus.exe.
//
// The plugin itself (TypeScript, owned by the adapter engineer) is not part
// of this repository yet: files/ only holds a README. Once its files are
// added to files/, go:embed bundles them and `plexus setup` installs them.
package plugin

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:files
var files embed.FS

// Dir is the plugin directory name under %DSH_HOME%\plugins.
const Dir = "plexus-bridge"

// Bundled reports whether this build carries the plugin (more than the
// placeholder README).
func Bundled() bool {
	n := 0
	_ = fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && p != "files/README.md" {
			n++
		}
		return nil
	})
	return n > 0
}

// Install writes the bundled plugin into dshHome/plugins/plexus-bridge and
// returns that directory. It overwrites older copies of the same files.
func Install(dshHome string) (string, error) {
	dst := filepath.Join(dshHome, "plugins", Dir)
	err := fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("files", filepath.FromSlash(p))
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	return dst, err
}
