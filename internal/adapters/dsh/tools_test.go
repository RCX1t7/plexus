package dsh

import (
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

// DSH-internal meta tools must map to ToolMeta so the single classifier never
// flags them (notably send_message must not read as an out.mcp egress), and
// everything else must stay unclassified here (Normalize decides).
func TestInternalKind(t *testing.T) {
	meta := []string{"send_message", "interrupt_agent", "list_agents", "workflow",
		"exit_plan_mode", "skill", "todo_write", "ask_user_question",
		"subagent", "subagent_start", "job_run", "job_123", "set_goal", "update_goal"}
	for _, n := range meta {
		if internalKind(n) != harness.ToolMeta {
			t.Errorf("%q: want Meta, got %q", n, internalKind(n))
		}
	}
	for _, n := range []string{"bash", "read_file", "write_file", "plexus_post", "web_fetch", ""} {
		if internalKind(n) != "" {
			t.Errorf("%q: want unclassified, got %q", n, internalKind(n))
		}
	}
}

// TestMapToolTypedTargets covers each DSH tool name through mapTool: the single
// classifier must see the right kind, the bash command, and every path/target
// the call touches -- including the DSH-specific "directory" and move "source"
// that harness.Normalize does not read, and DSH-internal tools pinned to Meta
// (CR-5).
func TestMapToolTypedTargets(t *testing.T) {
	cases := []struct {
		name     string
		input    map[string]any
		wantKind harness.ToolKind
		wantCmd  string
		wantPath []string
	}{
		// bash: typed command reaches the classifier (workdir is a separate
		// harness gap, see the report -- ToolRequest has no Workdir field).
		{"bash", map[string]any{"command": "rm -rf /tmp/x"}, harness.ToolShell, "rm -rf /tmp/x", nil},
		{"bash_persistent", map[string]any{"command": "ls"}, harness.ToolShell, "ls", nil},
		{"pwsh", map[string]any{"command": "Remove-Item x"}, harness.ToolShell, "Remove-Item x", nil},
		// str-replace-editor create/str_replace carry path.
		{"str_replace_based_edit_tool", map[string]any{"path": "/etc/hosts"}, harness.ToolWrite, "", []string{"/etc/hosts"}},
		{"create_file", map[string]any{"file_path": "/out/a.txt"}, harness.ToolWrite, "", []string{"/out/a.txt"}},
		// fs write with the DSH "target" (Normalize) and move "source" (added here).
		{"move_file", map[string]any{"source": "/in/a", "destination": "/out/a"}, harness.ToolWrite, "", []string{"/out/a", "/in/a"}},
		// a directory-typed write target Normalize does not read.
		{"create_directory", map[string]any{"directory": "/out/newdir"}, harness.ToolWrite, "", []string{"/out/newdir"}},
		// read family: classified read, path carried.
		{"read_file", map[string]any{"path": "/etc/passwd"}, harness.ToolRead, "", []string{"/etc/passwd"}},
		{"fs_search", map[string]any{"path": "/src", "pattern": "x"}, harness.ToolRead, "", []string{"/src"}},
		// DSH-internal meta tools are pinned to Meta regardless of input.
		{"send_message", map[string]any{"to": "agent-2", "text": "hi"}, harness.ToolMeta, "", nil},
		{"subagent_start", map[string]any{"goal": "do"}, harness.ToolMeta, "", nil},
		{"job_output", map[string]any{"jobId": "7"}, harness.ToolMeta, "", nil},
	}
	for _, c := range cases {
		got := mapTool(harness.ToolRequest{Name: c.name, Input: c.input})
		if got.Kind != c.wantKind {
			t.Errorf("%s: kind = %q, want %q", c.name, got.Kind, c.wantKind)
		}
		if got.Command != c.wantCmd {
			t.Errorf("%s: command = %q, want %q", c.name, got.Command, c.wantCmd)
		}
		if !samePaths(got.Paths, c.wantPath) {
			t.Errorf("%s: paths = %v, want %v", c.name, got.Paths, c.wantPath)
		}
	}
}

func samePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
