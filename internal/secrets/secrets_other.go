//go:build !windows

package secrets

import (
	"errors"
	"os"
	"path/filepath"
)

// Open refuses to store secrets in a plain file unless the developer opts
// in with PLEXUS_INSECURE_DEV_SECRETS=1 (Linux/macOS are not targets).
func Open(dir string) (Store, error) {
	if os.Getenv("PLEXUS_INSECURE_DEV_SECRETS") != "1" {
		return nil, errors.New("no OS secret store on this platform; set PLEXUS_INSECURE_DEV_SECRETS=1 for a 0600 dev file")
	}
	id := func(b []byte) ([]byte, error) { return b, nil }
	return &sealedFile{path: filepath.Join(dir, "secrets.dev.json"), seal: id, open: id}, nil
}
