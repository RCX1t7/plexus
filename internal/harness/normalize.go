package harness

import (
	"strings"
)

// Normalize fills Kind, Command and Paths of a tool request from its name
// and raw input when the adapter could not map them natively (DSH bridge,
// ACP agents). The single dangerous-action classifier in Plexus core reads
// exactly these fields, so every adapter's calls are classified the same way.
func Normalize(r ToolRequest) ToolRequest {
	in, _ := r.Input.(map[string]any)
	if r.Command == "" && in != nil {
		for _, k := range []string{"command", "cmd", "commandLine", "script"} {
			if c := joinArg(in[k]); c != "" {
				if args := joinArg(in["args"]); args != "" && k != "commandLine" {
					c += " " + args
				}
				r.Command = c
				break
			}
		}
	}
	if len(r.Paths) == 0 && in != nil {
		for _, k := range []string{"file_path", "filePath", "path", "notebook_path", "target", "destination"} {
			if p, ok := in[k].(string); ok && p != "" {
				r.Paths = append(r.Paths, p)
			}
		}
		if ps, ok := in["paths"].([]any); ok {
			for _, p := range ps {
				if s, ok := p.(string); ok && s != "" {
					r.Paths = append(r.Paths, s)
				}
			}
		}
	}
	if r.Workdir == "" && in != nil {
		// per-call working directory (DSH bash, MCP shells): relative paths
		// in the call resolve against it, not the session's workdir
		for _, k := range []string{"workdir", "cwd"} {
			if d, ok := in[k].(string); ok && strings.TrimSpace(d) != "" {
				r.Workdir = strings.TrimSpace(d)
				break
			}
		}
	}
	patchPaths(&r, in)
	if r.Kind == "" || r.Kind == ToolOther {
		n := strings.ToLower(r.Name)
		switch {
		case r.Command != "" || containsAny(n, "bash", "shell", "pwsh", "powershell", "exec", "terminal", "run_command"):
			r.Kind = ToolShell
		case containsAny(n, "write", "edit", "patch", "delete", "remove", "move", "rename", "create", "mkdir"):
			r.Kind = ToolWrite
		case containsAny(n, "read", "grep", "glob", "list", "search", "view", "ls"):
			if !strings.HasPrefix(n, "mcp") {
				r.Kind = ToolRead
			}
		case containsAny(n, "fetch", "web", "http", "browse"):
			r.Kind = ToolFetch
		}
		if r.Kind == "" {
			r.Kind = ToolOther
		}
	}
	if r.Kind != ToolShell && r.Kind != ToolRead && deleteName(r.Name) {
		// delete_file, fs_remove, trash, ... (MCP servers, DSH tools): every
		// path such a tool names is deleted.
		for _, p := range r.Paths {
			r.Deletes = addPath(r.Deletes, p)
		}
	}
	return r
}

func deleteName(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"delete", "remove", "unlink", "trash", "rmdir", "rmtree", "erase", "purge"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	for _, f := range strings.FieldsFunc(n, func(c rune) bool { return c == '_' || c == '-' || c == '.' || c == '/' }) {
		if f == "rm" || f == "del" {
			return true
		}
	}
	return false
}

func addPath(list []string, p string) []string {
	for _, x := range list {
		if x == p {
			return list
		}
	}
	return append(list, p)
}

// patchPaths reads apply_patch envelopes (Codex, and any harness that
// forwards one) in the command or the input: "*** Delete File: p" is a
// delete, "*** Update File: p" + "*** Move to: q" moves p away (a delete
// of p and a write of q), Add/Update are writes.
func patchPaths(r *ToolRequest, in map[string]any) {
	texts := []string{r.Command}
	if s, ok := r.Input.(string); ok {
		texts = append(texts, s)
	}
	for _, k := range []string{"patch", "input", "content", "diff", "changes"} {
		if s, ok := in[k].(string); ok {
			texts = append(texts, s)
		}
	}
	for _, t := range texts {
		if !strings.Contains(t, "*** ") {
			continue
		}
		last := ""
		for _, line := range strings.Split(t, "\n") {
			line = strings.TrimSpace(line)
			for _, h := range []string{"*** Delete File:", "*** Add File:", "*** Update File:", "*** Move to:"} {
				if !strings.HasPrefix(line, h) {
					continue
				}
				p := strings.TrimSpace(line[len(h):])
				if p == "" {
					continue
				}
				r.Paths = addPath(r.Paths, p)
				switch h {
				case "*** Delete File:":
					r.Deletes = addPath(r.Deletes, p)
				case "*** Move to:":
					if last != "" {
						r.Deletes = addPath(r.Deletes, last)
					}
				}
				last = p
			}
		}
		if r.Kind == "" || r.Kind == ToolOther {
			r.Kind = ToolWrite
		}
	}
}

func joinArg(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		var parts []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
