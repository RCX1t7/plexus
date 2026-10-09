package handoff

import (
	"strings"
	"testing"
)

func TestParseFillCard(t *testing.T) {
	msg := "<@U2PARTNER> can you take this?\nHANDOFF\ntask: Add retries to the uploader\ninputs: uploader.go; issue #12\n" +
		"done_when: go test ./uploader passes\nevidence: timeout after 30s\ntried_failed: raising the timeout\n"
	r, rest, ok := Parse(msg)
	if !ok {
		t.Fatal("no block parsed")
	}
	if r.Task != "Add retries to the uploader" || len(r.Inputs) != 2 || r.DoneWhen == "" || len(r.TriedFailed) != 1 {
		t.Fatalf("bad record %+v", r)
	}
	if strings.Contains(rest, "HANDOFF") || !strings.Contains(rest, "can you take this") {
		t.Fatalf("rest = %q", rest)
	}
	if m := r.Missing(); len(m) != 1 || m[0] != "owner_if_stuck" {
		t.Fatalf("missing = %v", m)
	}
	r.Fill(rest, "U1SIN")
	if r.OwnerIfStuck != "<@U1SIN>" || len(r.Missing()) != 0 {
		t.Fatalf("fill: %+v", r)
	}
	card := r.Card("C1:1.2/handoff", "U2PARTNER")
	for _, want := range []string{"Handoff", "<@U2PARTNER>", "Done when:", "If stuck:", "uploader.go · issue #12"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card lacks %q:\n%s", want, card)
		}
	}
	if !strings.Contains(r.Prompt(""), `"done_when":"go test ./uploader passes"`) {
		t.Fatal(r.Prompt(""))
	}
}

func TestNoBlock(t *testing.T) {
	if _, _, ok := Parse("just a reply"); ok {
		t.Fatal("parsed a block from plain text")
	}
	var r Record
	r.Fill("first line\nsecond", "")
	if r.Task != "first line" {
		t.Fatal(r.Task)
	}
	if !strings.Contains(r.Card("", ""), "_missing") {
		t.Fatal("missing fields must be visible on the card")
	}
}
