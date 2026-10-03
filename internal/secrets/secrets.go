// Package secrets keeps Slack tokens and the hub signing key out of plain
// files and logs. On Windows the whole map is encrypted with DPAPI
// (CryptProtectData, bound to the current Windows user) into
// %APPDATA%\Plexus\secrets.dpapi. Other OSes are development only.
package secrets

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound is returned for a missing key.
var ErrNotFound = errors.New("secret not found")

// Store is a small key/value secret store.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// Memory is an in-process store for tests.
type Memory struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *Memory) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *Memory) Set(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[k] = v
	return nil
}

func (s *Memory) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, k)
	return nil
}

// sealedFile stores the map in one file, transformed by seal/open.
type sealedFile struct {
	path       string
	seal, open func([]byte) ([]byte, error)
	mu         sync.Mutex
}

func (f *sealedFile) load() (map[string]string, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := f.open(b)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	return m, json.Unmarshal(plain, &m)
}

func (f *sealedFile) save(m map[string]string) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b, err := f.seal(plain)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

func (f *sealedFile) Get(k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return "", err
	}
	v, ok := m[k]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *sealedFile) Set(k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	m[k] = v
	return f.save(m)
}

func (f *sealedFile) Delete(k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	delete(m, k)
	return f.save(m)
}

// Keys used by Plexus.
func BotTokenKey(bot string) string { return "bot/" + bot + "/bot_token" }
func AppTokenKey(bot string) string { return "bot/" + bot + "/app_token" }
