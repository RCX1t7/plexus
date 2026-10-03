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
