package harness

import (
	"bytes"
	"strings"
	"testing"
)

func frame(n int) []byte {
	b := bytes.Repeat([]byte("x"), n)
	b[0] = '{'
	return b
}

func TestReadFramesCap(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(MaxFrame)) // exactly the cap: kept
	in.WriteString("\n")
	in.Write(frame(MaxFrame + 1)) // one byte over: dropped
	in.WriteString("\n")
	in.WriteString(`{"after":1}` + "\r\n") // recovery
	in.Write(frame(MaxFrame + 70000))      // over by more than a read buffer
	in.WriteString("\n" + `{"last":2}`)    // no trailing newline at EOF
	var got []int
	var drops []int64
	readFrames(&in, MaxFrame, func(b []byte) { got = append(got, len(b)) }, func(n int64) { drops = append(drops, n) })
	if len(got) != 3 || got[0] != MaxFrame || got[1] != len(`{"after":1}`) || got[2] != len(`{"last":2}`) {
		t.Fatalf("lines %v", got)
	}
	if len(drops) != 2 || drops[0] != MaxFrame+1 || drops[1] != MaxFrame+70000 {
		t.Fatalf("drops %v", drops)
	}
}

func TestReadFramesSmall(t *testing.T) {
	in := strings.NewReader("{a}\n{bbbbbb}\n\n{c}\n")
	var got []string
	n := 0
	readFrames(in, 4, func(b []byte) { got = append(got, string(b)) }, func(int64) { n++ })
	if strings.Join(got, ",") != "{a},{c}" || n != 1 {
		t.Fatal(got, n)
	}
}
