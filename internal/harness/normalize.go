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
	return r
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
