package wsmini

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEchoRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteText(append([]byte("echo:"), msg...))
		}
	}))
	defer srv.Close()
	c, err := Dial("ws" + strings.TrimPrefix(srv.URL, "http") + "/x?y=1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, size := range []int{0, 5, 125, 126, 70000} {
		msg := bytes.Repeat([]byte("a"), size)
		if err := c.WriteText(msg); err != nil {
			t.Fatal(err)
		}
		if err := c.Ping(); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.ReadMessage()
		if err != nil || op != OpText || !bytes.Equal(got, append([]byte("echo:"), msg...)) {
			t.Fatalf("size %d: op=%d err=%v len=%d", size, op, err, len(got))
		}
	}
	_ = c.CloseGracefully()
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("expected error after close")
	} else if err != io.EOF && !strings.Contains(err.Error(), "closed") {
		t.Logf("read after close: %v", err)
	}
}
