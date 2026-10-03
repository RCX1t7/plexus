package dsh

import (
	"strings"

	"github.com/RCX1t7/plexus/internal/harness"
)

// dshInternalTools are DSH's own meta / orchestration tools. They have no
// side effect Plexus gates (messaging between agents, planning, todo
// bookkeeping, sub-agent, job and goal control, skills, asking the user), so
// they are mapped to harness.ToolMeta. That keeps the single dangerous-action
// classifier from mistaking them for something dangerous (notably
// send_message must not be read as an out.mcp egress), and they are never
// parked. The bridge additionally skips forwarding them for approval.
var dshInternalTools = map[string]bool{
	"send_message":      true,
	"interrupt_agent":   true,
	"list_agents":       true,
	"workflow":          true,
	"exit_plan_mode":    true,
	"skill":             true,
	"todo_write":        true,
	"ask_user_question": true,
}

// internalKind returns harness.ToolMeta for a DSH-internal tool, matching the
// exact names above and the subagent* / job_* / *_goal families; it returns ""
// for everything else (so Normalize classifies it as usual).
func internalKind(name string) harness.ToolKind {
	if dshInternalTools[name] ||
		strings.HasPrefix(name, "subagent") ||
		strings.HasPrefix(name, "job_") ||
		strings.HasSuffix(name, "_goal") {
		return harness.ToolMeta
	}
	return ""
}
