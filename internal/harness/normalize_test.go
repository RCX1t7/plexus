package harness

import "testing"

func TestNormalize(t *testing.T) {
	r := Normalize(ToolRequest{Name: "bash", Input: map[string]any{"command": "git -C x push -f"}})
	if r.Kind != ToolShell || r.Command != "git -C x push -f" {
		t.Fatalf("%+v", r)
	}
	r = Normalize(ToolRequest{Name: "run", Input: map[string]any{"command": []any{"git", "push", "--force"}}})
	if r.Kind != ToolShell || r.Command != "git push --force" {
		t.Fatalf("%+v", r)
	}
	r = Normalize(ToolRequest{Name: "delete_file", Input: map[string]any{"path": "/etc/hosts"}})
	if r.Kind != ToolWrite || len(r.Paths) != 1 {
		t.Fatalf("%+v", r)
	}
	r = Normalize(ToolRequest{Name: "Bash", Kind: ToolShell, Command: "ls"})
	if r.Command != "ls" || r.Kind != ToolShell {
		t.Fatalf("%+v", r)
	}
}
