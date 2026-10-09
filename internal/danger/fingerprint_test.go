package danger

import (
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

// B2: "approve once" must only let the identical call through. The
// fingerprint covers the canonical input, the call's workdir and its
// delete targets; per-call ids do not change it.
func TestFingerprintDistinguishesCalls(t *testing.T) {
	send := func(ch, text, callID string) Call {
		return Call{Kind: harness.ToolOther, Name: "mcp__slack__send_message",
			Input: map[string]any{"channel": ch, "text": text, "call_id": callID, "meta": map[string]any{"requestId": callID, "thread": "1.0"}}}
	}
	rm := func(wd string) Call {
		return Call{Kind: harness.ToolShell, Name: "bash", Command: "rm -rf x", Workdir: wd}
	}
	del := func(p string) Call {
		return Call{Kind: harness.ToolWrite, Name: "fileChange", Paths: []string{"a.go"}, Deletes: []string{p}}
	}
	same := []struct {
		name string
		a, b Call
	}{
		{"identical send, new call ids", send("C_SIN", "hi", "c1"), send("C_SIN", "hi", "c2")},
		{"identical rm", rm("/tmp"), rm("/tmp")},
		{"map key order", Call{Name: "t", Input: map[string]any{"a": 1, "b": []any{"x", 2}}}, Call{Name: "t", Input: map[string]any{"b": []any{"x", 2}, "a": 1}}},
		{"struct vs map input", Call{Name: "t", Input: struct {
			A string `json:"a"`
		}{"v"}}, Call{Name: "t", Input: map[string]any{"a": "v"}}},
		{"command whitespace", Call{Kind: harness.ToolShell, Command: "git push --force"}, Call{Kind: harness.ToolShell, Command: "git  push   --force"}},
	}
	for _, c := range same {
		if Fingerprint(c.a) != Fingerprint(c.b) {
			t.Errorf("%s: fingerprints differ", c.name)
		}
	}
	diff := []struct {
		name string
		a, b Call
	}{
		{"send to another recipient", send("C_SIN", "hi", "c1"), send("C_PUBLIC", "hi", "c1")},
		{"send other text", send("C_SIN", "hi", "c1"), send("C_SIN", "leak", "c1")},
		{"rm in another workdir", rm("/tmp"), rm("/home/u")},
		{"rm with and without workdir", rm(""), rm("/home/u")},
		{"other delete target", del("../a"), del("../b")},
	}
	for _, c := range diff {
		if Fingerprint(c.a) == Fingerprint(c.b) {
			t.Errorf("%s: same fingerprint %q", c.name, Fingerprint(c.a))
		}
	}
}
