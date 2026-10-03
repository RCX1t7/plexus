package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The control endpoint lets `plexus stop` reach the running hub (bbolt
// locks the database file, so the CLI cannot open it while the hub runs).
// It listens on 127.0.0.1 only, on a random port, and requires a random
// nonce that is written to DataDir/control.json (mode 0600; on Windows the
// file inherits the per-user ACL of %LOCALAPPDATA%).
type control struct {
	Port  int    `json:"port"`
	Nonce string `json:"nonce"`
	PID   int    `json:"pid"`
}

// ControlFile is the path of the control endpoint description.
func ControlFile(dataDir string) string { return filepath.Join(dataDir, "control.json") }

func (h *Hub) serveControl(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		ln.Close()
		return err
	}
	c := &control{Port: ln.Addr().(*net.TCPAddr).Port, Nonce: hex.EncodeToString(b), PID: os.Getpid()}
	data, _ := json.Marshal(c)
	path := ControlFile(h.DataDir)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		ln.Close()
		return err
	}
	h.control = c
	mux := http.NewServeMux()
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Plexus-Nonce")), []byte(c.Nonce)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		root := strings.TrimSpace(string(body))
		if root == "" {
			http.Error(w, "task id missing", http.StatusBadRequest)
			return
		}
		n, err := h.StopTask(r.Context(), root, "cli")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%d", n)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
		_ = os.Remove(path)
	}()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// ErrNoHub means no running hub answered on the control endpoint.
var ErrNoHub = errors.New("no running plexus hub")

// RemoteStop asks the running hub to stop a task tree. It returns ErrNoHub
// when no hub is running (the caller may then revoke in the store directly).
func RemoteStop(dataDir, root string) (int, error) {
	data, err := os.ReadFile(ControlFile(dataDir))
	if err != nil {
		return 0, ErrNoHub
	}
	var c control
	if json.Unmarshal(data, &c) != nil || c.Port == 0 {
		return 0, ErrNoHub
	}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/stop", c.Port), strings.NewReader(root))
	req.Header.Set("X-Plexus-Nonce", c.Nonce)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, ErrNoHub // stale control.json
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("hub refused the stop: %s", strings.TrimSpace(string(body)))
	}
	var n int
	fmt.Sscan(string(body), &n)
	return n, nil
}
