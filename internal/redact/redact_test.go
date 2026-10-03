package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	in := "bot xoxb-1234567890-abcdefghij and OPENAI_API_KEY=sk-abcdefghijklmnop1234 and sha 3f786850e387550fdab836ed7e6dc881de23001b"
	out := String(in, "my-live-secret-123")
	if strings.Contains(out, "xoxb-1234567890") || strings.Contains(out, "sk-abcdefghijklmnop1234") {
		t.Fatalf("not redacted: %s", out)
	}
	if !strings.Contains(out, "OPENAI_API_KEY=") || !strings.Contains(out, "3f786850e387550fdab836ed7e6dc881de23001b") {
		t.Fatalf("over-redacted: %s", out)
	}
	if strings.Contains(String("token my-live-secret-123 here", "my-live-secret-123"), "my-live-secret-123") {
		t.Fatal("extra secret kept")
	}
}

func TestWriter(t *testing.T) {
	var b bytes.Buffer
	w := &Writer{W: &b}
	w.AddSecret("xapp-1-A0-123-abcdefabcdef")
	_, _ = w.Write([]byte("connecting with xapp-1-A0-123-abcdefabcdef\n"))
	if strings.Contains(b.String(), "abcdefabcdef") {
		t.Fatal(b.String())
	}
}
