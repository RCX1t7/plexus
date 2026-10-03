package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RCX1t7/plexus/internal/slackbot"
	"github.com/RCX1t7/plexus/internal/store"
)

func TestControlEndpointStop(t *testing.T) {
	dir := t.TempDir()
	if _, err := RemoteStop(dir, "C1:1.0"); !errors.Is(err, ErrNoHub) {
		t.Fatalf("no hub: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "plexus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &Hub{DataDir: dir, Store: st, Stops: &slackbot.Stops{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.serveControl(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteStop(dir, "C1:1.0"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.AnyRevoked("C1:1.0"); !r {
		t.Fatal("not stopped")
	}
	// a wrong nonce is refused
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/stop", h.control.Port), strings.NewReader("C9:9"))
	req.Header.Set("X-Plexus-Nonce", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal(resp.StatusCode)
	}
	if r, _ := st.AnyRevoked("C9:9"); r {
		t.Fatal("stopped with a wrong nonce")
	}
}
