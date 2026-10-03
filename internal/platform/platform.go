// Package platform hides the OS-specific bits (Windows is the target, Linux
// is used for development and tests).
package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ConfigDir is where config.json and the encrypted secret store live:
// %APPDATA%\Plexus on Windows, $XDG_CONFIG_HOME/plexus elsewhere.
// PLEXUS_HOME overrides both ConfigDir and DataDir (used by tests).
func ConfigDir() string {
	if h := os.Getenv("PLEXUS_HOME"); h != "" {
		return h
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(d, "Plexus")
	}
	return filepath.Join(d, "plexus")
}

// DataDir holds the bbolt database (plexus.db): %LOCALAPPDATA%\Plexus on Windows.
func DataDir() string {
	if h := os.Getenv("PLEXUS_HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			return filepath.Join(l, "Plexus")
		}
	}
	return ConfigDir()
}

// CaseInsensitivePaths reports whether the file system compares names
// case-insensitively (true on Windows).
func CaseInsensitivePaths(goos string) bool { return goos == "windows" }

// NormPath cleans p for comparison: forward/back slashes unified on Windows,
// lower-cased when the FS is case-insensitive.
func NormPath(goos, p string) string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, "/", `\`)
		return strings.ToLower(filepath.Clean(p))
	}
	return filepath.Clean(p)
}
