package redact

import (
	"io"
	"sync"
)

// Writer redacts every write (used under the log handler, so nothing that
// reaches a log file or console carries a token).
type Writer struct {
	W     io.Writer
	mu    sync.Mutex
	extra []string
}

// AddSecret registers an exact secret to always mask.
func (w *Writer) AddSecret(s string) {
	w.mu.Lock()
	w.extra = append(w.extra, s)
	w.mu.Unlock()
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	extra := append([]string(nil), w.extra...)
	w.mu.Unlock()
	if _, err := io.WriteString(w.W, String(string(p), extra...)); err != nil {
		return 0, err
	}
	return len(p), nil
}
