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

// movePathArgs are path-typed argument names DSH tools use that
// harness.Normalize does not already read (it knows file_path/filePath/path/
// notebook_path/target/destination/paths). DSH's fs/str-replace-editor tools
// also carry a "directory" target and move/rename "source", so we add those to
// the typed targets after Normalize, giving the single Go classifier every
// path a non-read call touches (CR-5). Harmless on read kinds (the classifier
// only gates writes/shells by path); on a write/move it lets the classifier
// see an out-of-workspace target it would otherwise miss.
var movePathArgs = []string{"directory", "source", "old_path", "new_path", "dir", "src", "dst"}

// mapTool builds the classifier-facing tool request for a DSH tool call: pin
// DSH-internal meta tools to ToolMeta, let harness.Normalize derive
// kind/command/paths from the name and input, then add the DSH-specific path
// arguments Normalize does not know. This is the single place the adapter
// shapes a DSH tool call before the core classifier sees it.
func mapTool(tr harness.ToolRequest) harness.ToolRequest {
	if k := internalKind(tr.Name); k != "" {
		tr.Kind = k
	}
	if tr.Workdir == "" {
		tr.Workdir = callWorkdir(tr.Input)
	}
	tr = harness.Normalize(tr)
	if in, ok := tr.Input.(map[string]any); ok {
		for _, k := range movePathArgs {
			p, ok := in[k].(string)
			if !ok || p == "" {
				continue
			}
			dup := false
			for _, have := range tr.Paths {
				if have == p {
					dup = true
					break
				}
			}
			if !dup {
				tr.Paths = append(tr.Paths, p)
			}
		}
	}
	return tr
}

// cwdArgs are the per-call working-directory arguments DSH tools take: bash,
// bash_persistent, pwsh and pwsh_persistent accept "workdir" ("pass workdir
// instead of using cd"); "cwd" covers MCP-style shells. DSH resolves a
// relative one against the session workspace, which is exactly how the
// classifier resolves ToolRequest.Workdir (against Ctx.Workdir).
var cwdArgs = []string{"workdir", "cwd"}

// callWorkdir returns the call's own working directory ("" = the session's),
// so relative paths in the command resolve where DSH will actually run it.
func callWorkdir(input any) string {
	in, _ := input.(map[string]any)
	for _, k := range cwdArgs {
		if d, ok := in[k].(string); ok && strings.TrimSpace(d) != "" {
			return strings.TrimSpace(d)
		}
	}
	return ""
}
