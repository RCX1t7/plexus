package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSealedFileRoundTrip(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Setenv("PLEXUS_INSECURE_DEV_SECRETS", "1")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(BotTokenKey("a")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Set(BotTokenKey("a"), "xoxb-1"); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(dir)
	if v, err := s2.Get(BotTokenKey("a")); err != nil || v != "xoxb-1" {
		t.Fatal(v, err)
	}
	if err := s2.Delete(BotTokenKey("a")); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "secrets*"))
	if fi, _ := os.Stat(files[0]); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
}

func TestRefusesPlainFileByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	t.Setenv("PLEXUS_INSECURE_DEV_SECRETS", "")
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("plain file store opened without opt-in")
	}
}
