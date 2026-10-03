// Package danger classifies tool calls that need Sin's explicit approval
// before they run (the one approval gate between Sin and the partners).
//
// Classify is a pure function: callers pass in every fact it needs (git
// facts included), so it is table-tested without side effects. It only sees
// the command line: what a script or build tool does internally
// (./deploy.sh, make release, npm run x) is invisible to it.
package danger

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/RCX1t7/plexus/internal/harness"
)

// Call is one normalized tool call.
type Call struct {
	Kind    harness.ToolKind
	Name    string   // native tool name (MCP tools: their full name)
	Command string   // shell command line, if any
	Paths   []string // files the call writes or deletes
}

// Ctx carries the facts Classify may use.
type Ctx struct {
	GOOS    string
	Workdir string
	// HeadPushed reports whether the current branch's HEAD is already on
	// its upstream (rebase/reset/amend then rewrite published history).
	HeadPushed func() bool
}

// Rules is the dangerous_actions config.
type Rules struct {
	UseDefaults   *bool    `json:"use_defaults,omitempty"`    // default true
	Disable       []string `json:"disable,omitempty"`         // rule ids (or prefixes like "sys.")
	ExtraCommands []string `json:"extra_commands,omitempty"`  // regexes on each command segment
	ExtraMCPTools []string `json:"extra_mcp_tools,omitempty"` // regexes on MCP tool names
}

// Hit names the rule a call matched.
type Hit struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

func (r Rules) defaults() bool { return r.UseDefaults == nil || *r.UseDefaults }

func (r Rules) disabled(rule string) bool {
	for _, d := range r.Disable {
		if d == rule || (strings.HasSuffix(d, ".") && strings.HasPrefix(rule, d)) {
			return true
		}
	}
	return false
}

// Classify returns the matched rule, or nil.
func Classify(c Call, x Ctx, r Rules) *Hit {
	var hits []*Hit
	if r.defaults() {
		for _, seg := range Segments(c.Command) {
			hits = append(hits, classifyArgv(seg, x))
		}
		if c.Kind == harness.ToolWrite {
			for _, p := range c.Paths {
				if systemDir(p, x.GOOS) {
					hits = append(hits, &Hit{"sys.dir", "writes into a system directory: " + p})
				}
			}
		}
		if c.Kind == harness.ToolOther && mcpSend.MatchString(c.Name) {
			hits = append(hits, &Hit{"out.mcp", "tool " + c.Name + " looks like it sends a message"})
		}
	}
	for _, e := range r.ExtraCommands {
		if re, err := regexp.Compile(e); err == nil {
			for _, seg := range Segments(c.Command) {
				if re.MatchString(strings.Join(seg, " ")) {
					hits = append(hits, &Hit{"extra.command", "matches " + e})
				}
			}
		}
	}
	for _, e := range r.ExtraMCPTools {
		if re, err := regexp.Compile(e); err == nil && c.Name != "" && c.Kind == harness.ToolOther && re.MatchString(c.Name) {
			hits = append(hits, &Hit{"extra.mcp", "matches " + e})
		}
	}
	for _, h := range hits {
		if h != nil && !r.disabled(h.Rule) {
			return h
		}
	}
	return nil
}

var mcpSend = regexp.MustCompile(`(?i)(send|post|reply|forward)[_-]?(message|mail|email|dm|chat|tweet)|chat[_.]?postmessage|gmail|outlook|slack_send|send_draft|webhook`)

// Segments splits a command line into simple commands (on ; && || | and
// newlines), unwrapping cmd /c, powershell -Command / -EncodedCommand and
// sh -c, and dropping leading VAR=value assignments.
func Segments(cmd string) [][]string {
	var out [][]string
	for _, part := range splitOps(cmd) {
		argv := Fields(part)
		for len(argv) > 0 && strings.Contains(argv[0], "=") && !strings.HasPrefix(argv[0], "-") && !strings.ContainsAny(argv[0], `/\`) {
			argv = argv[1:]
		}
		if len(argv) == 0 {
			continue
		}
		if inner, ok := unwrap(argv); ok {
			out = append(out, Segments(inner)...)
			continue
		}
		out = append(out, argv)
	}
	return out
}

func exeName(s string) string {
	s = strings.ToLower(filepath.Base(strings.ReplaceAll(s, `\`, "/")))
	return strings.TrimSuffix(s, ".exe")
}

func unwrap(argv []string) (string, bool) {
	switch exeName(argv[0]) {
	case "cmd":
		for i, a := range argv[1:] {
			if l := strings.ToLower(a); l == "/c" || l == "/k" {
				return strings.Join(argv[i+2:], " "), true
			}
		}
	case "powershell", "pwsh":
		for i := 1; i < len(argv); i++ {
			l := strings.ToLower(argv[i])
			switch {
			case l == "-encodedcommand" || l == "-enc" || l == "-ec" || l == "-e":
				if i+1 < len(argv) {
					return decodePS(argv[i+1]), true
				}
			case l == "-command" || l == "-c":
				return strings.Join(argv[i+1:], " "), true
			case !strings.HasPrefix(l, "-"):
				return strings.Join(argv[i:], " "), true
			case l == "-executionpolicy" || l == "-file" || l == "-windowstyle":
				i++
			}
		}
	case "bash", "sh", "zsh", "dash":
		for i, a := range argv[1:] {
			if a == "-c" || (strings.HasPrefix(a, "-") && strings.Contains(a, "c") && !strings.HasPrefix(a, "--")) {
				return strings.Join(argv[i+2:], " "), true
			}
		}
	}
	return "", false
}

func decodePS(b64 string) string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw)%2 != 0 {
		return "UNDECODABLE-ENCODED-COMMAND"
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

// splitOps splits on unquoted ; && || | & and newlines.
func splitOps(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
			cur.WriteRune(c)
		case c == '"' || c == '\'':
			q = c
			cur.WriteRune(c)
		case c == ';' || c == '\n' || c == '|' || c == '&':
			out = append(out, cur.String())
			cur.Reset()
			if i+1 < len(rs) && (rs[i+1] == c) {
				i++
			}
		default:
			cur.WriteRune(c)
		}
	}
	return append(out, cur.String())
}

// Fields splits on whitespace honoring simple quotes.
func Fields(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	in := false
	for _, c := range s {
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			q, in = c, true
		case c == ' ' || c == '\t' || c == '\r':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func classifyArgv(argv []string, x Ctx) *Hit {
	exe := exeName(argv[0])
	args := argv[1:]
	low := make([]string, len(args))
	for i, a := range args {
		low[i] = strings.ToLower(a)
	}
	line := strings.ToLower(strings.Join(argv, " "))
	switch exe {
	case "git":
		return gitRule(args, x)
	case "bfg":
		return &Hit{"git.rewrite", "bfg rewrites history"}
	case "rm", "del", "erase", "rd", "rmdir", "remove-item", "ri", "rimraf", "unlink", "shred":
		for _, p := range args {
			if strings.HasPrefix(p, "-") || (strings.HasPrefix(p, "/") && len(p) == 2 && x.GOOS == "windows") {
				continue
			}
			if outside(p, x) {
				return &Hit{"fs.delete_outside", "deletes outside the workdir: " + p}
			}
			if systemDir(p, x.GOOS) {
				return &Hit{"sys.dir", "deletes in a system directory: " + p}
			}
		}
	case "reg":
		if len(low) > 0 && oneOf(low[0], "add", "delete", "import", "restore", "load", "unload", "copy") {
			return &Hit{"sys.registry", "changes the registry"}
		}
	case "set-itemproperty", "new-itemproperty", "remove-itemproperty", "new-item", "set-item", "rename-itemproperty":
		if strings.Contains(line, "hklm:") || strings.Contains(line, "hkcu:") || strings.Contains(line, "registry::") {
			return &Hit{"sys.registry", "changes the registry"}
		}
	case "sc":
		if len(low) > 0 && oneOf(low[0], "create", "delete", "config", "stop", "start", "failure") {
			return &Hit{"sys.service", "changes a Windows service"}
		}
	case "new-service", "set-service", "remove-service", "stop-service", "start-service", "restart-service":
		return &Hit{"sys.service", "changes a Windows service"}
	case "schtasks":
		if containsAny(low, "/create", "/delete", "/change") {
			return &Hit{"sys.task", "changes a scheduled task"}
		}
	case "register-scheduledtask", "unregister-scheduledtask", "set-scheduledtask":
		return &Hit{"sys.task", "changes a scheduled task"}
	case "setx":
		if containsAny(low, "/m") {
			return &Hit{"sys.env", "sets a machine-wide environment variable"}
		}
	case "msiexec", "bcdedit":
		return &Hit{"sys.installer", exe + " changes the system"}
	case "winget", "choco", "apt", "apt-get", "dnf", "yum":
		if len(low) > 0 && oneOf(low[0], "install", "uninstall", "upgrade", "remove") {
			return &Hit{"sys.installer", exe + " " + low[0]}
		}
	case "send-mailmessage", "sendmail", "mail", "mutt", "mailx":
		return &Hit{"out.mail", "sends email"}
	case "curl", "wget", "invoke-webrequest", "iwr", "invoke-restmethod", "irm", "http", "xh":
		for _, a := range args {
			if webhook(a) {
				return &Hit{"out.webhook", "posts to a webhook: " + hostOf(a)}
			}
		}
	}
	if strings.Contains(line, "setenvironmentvariable") && strings.Contains(line, "machine") {
		return &Hit{"sys.env", "sets a machine-wide environment variable"}
	}
	if strings.HasSuffix(exe, ".msi") || (strings.Contains(exe, "setup") || strings.HasPrefix(exe, "install")) && strings.HasSuffix(strings.ToLower(argv[0]), ".exe") {
		return &Hit{"sys.installer", "runs an installer"}
	}
	return nil
}

func gitRule(args []string, x Ctx) *Hit {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") { // global options
		if args[i] == "-C" || args[i] == "-c" || args[i] == "--git-dir" || args[i] == "--work-tree" {
			i++
		}
		i++
	}
	if i >= len(args) {
		return nil
	}
	sub, rest := args[i], args[i+1:]
	pushed := x.HeadPushed != nil && x.HeadPushed()
	switch sub {
	case "push":
		pos := 0
		for _, a := range rest {
			switch {
			case a == "-f" || a == "--force" || strings.HasPrefix(a, "--force-with-lease") || a == "--force-if-includes" ||
				a == "--mirror" || a == "--delete" || a == "-d" || a == "--prune":
				return &Hit{"git.force", "git push " + a}
			case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "f"):
				return &Hit{"git.force", "git push " + a}
			case !strings.HasPrefix(a, "-"):
				pos++
				if pos >= 2 && (strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":")) {
					return &Hit{"git.force", "git push refspec " + a}
				}
			}
		}
	case "filter-branch", "filter-repo":
		return &Hit{"git.rewrite", "git " + sub + " rewrites history"}
	case "rebase", "reset":
		if pushed && !(sub == "rebase" && containsAny(rest, "--abort", "--continue", "--skip")) && !(sub == "reset" && len(rest) == 0) {
			return &Hit{"git.rewrite", "git " + sub + " on a pushed branch"}
		}
	case "commit":
		if pushed && containsAny(rest, "--amend") {
			return &Hit{"git.rewrite", "git commit --amend on a pushed branch"}
		}
	}
	return nil
}

var unexpanded = regexp.MustCompile(`^~|\$|%[A-Za-z_]+%`)

func outside(p string, x Ctx) bool {
	if unexpanded.MatchString(p) {
		return true // cannot tell where it points
	}
	win := x.GOOS == "windows"
	norm := func(s string) string {
		if win { // Windows semantics on any host: case-insensitive, either slash
			s = strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
		}
		return path.Clean(s)
	}
	root := norm(x.Workdir)
	a := norm(p)
	isAbs := strings.HasPrefix(a, "/") || (win && len(a) > 1 && a[1] == ':')
	if !isAbs {
		a = path.Join(root, a)
	}
	return a != root && !strings.HasPrefix(a, strings.TrimSuffix(root, "/")+"/")
}

func systemDir(p, goos string) bool {
	l := strings.ToLower(strings.ReplaceAll(p, "/", `\`))
	if goos == "windows" {
		for _, d := range []string{`c:\windows`, `c:\program files`, `c:\programdata`, `%windir%`, `%systemroot%`, `%programfiles%`} {
			if l == d || strings.HasPrefix(l, d+`\`) || strings.HasPrefix(l, d+` (x86)`) {
				return true
			}
		}
		return false
	}
	for _, d := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/lib", "/system"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

var webhookHosts = regexp.MustCompile(`(?i)^(hooks\.slack\.com|discord(app)?\.com|[a-z0-9-]+\.webhook\.office\.com|outlook\.office(365)?\.com|api\.telegram\.org|hooks\.zapier\.com|maker\.ifttt\.com)$`)

func webhook(a string) bool {
	h := hostOf(a)
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "discord.com") || strings.EqualFold(h, "discordapp.com") {
		return strings.Contains(strings.ToLower(a), "/api/webhooks")
	}
	return webhookHosts.MatchString(h)
}

func hostOf(a string) string {
	a = strings.Trim(a, `"'`)
	if !strings.Contains(a, "://") {
		return ""
	}
	u, err := url.Parse(a)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func oneOf(s string, v ...string) bool {
	for _, x := range v {
		if s == x {
			return true
		}
	}
	return false
}

func containsAny(list []string, v ...string) bool {
	for _, a := range list {
		if oneOf(strings.ToLower(a), v...) {
			return true
		}
	}
	return false
}

// Fingerprint identifies a call for one-time pre-approval after a restart.
func Fingerprint(c Call) string {
	return fmt.Sprintf("%s|%s|%s|%s", c.Kind, c.Name, strings.Join(strings.Fields(c.Command), " "), strings.Join(c.Paths, ";"))
}
