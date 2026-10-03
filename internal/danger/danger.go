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
	"sort"
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
	Deletes []string // the subset of files the call deletes (or moves away)
	Input   any      // raw native input (recipient lookup only)
}

// Ctx carries the facts Classify may use.
type Ctx struct {
	GOOS    string
	Workdir string
	// Git runs git with args in dir and returns its trimmed output and
	// whether it succeeded. It names targets (remote URL, branch) and tells
	// whether HEAD is pushed. nil: no git facts (targets fall back to names).
	Git func(dir string, args ...string) (string, bool)
	// HeadPushed overrides the Git-based check that the branch's HEAD is
	// already on its upstream (rebase/reset/amend then rewrite published
	// history). Tests use it.
	HeadPushed func(dir string) bool
}

// Rules is the dangerous_actions config.
type Rules struct {
	UseDefaults   *bool    `json:"use_defaults,omitempty"`    // default true
	Disable       []string `json:"disable,omitempty"`         // rule ids (or prefixes like "sys.")
	ExtraCommands []string `json:"extra_commands,omitempty"`  // regexes on each command segment
	ExtraMCPTools []string `json:"extra_mcp_tools,omitempty"` // regexes on MCP tool names
}

// Hit names the rule a call matched and its normalized target: the remote
// URL and branch for git rules, the absolute path for deletes, the
// recipient (host and path, address or tool) for out.* rules. Approvals
// "for this task" are granted and merged by (Rule, Target).
type Hit struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
	Target string `json:"target,omitempty"`
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

// shell tracks the working directory across the segments of one command
// line (cd / && rm -rf etc/x). cwd "" means unknown.
type shell struct{ cwd string }

// Classify returns the matched rule, or nil.
func Classify(c Call, x Ctx, r Rules) *Hit {
	var hits []*Hit
	if r.defaults() {
		if c.Kind == harness.ToolShell && strings.TrimSpace(c.Command) == "" {
			// an execute call whose command the adapter could not read
			hits = append(hits, &Hit{"exec.opaque", "runs a command Plexus cannot see (" + c.Name + ")", c.Name})
		}
		if l := strings.ToLower(c.Command); strings.Contains(l, "setenvironmentvariable") && strings.Contains(l, "machine") {
			hits = append(hits, &Hit{"sys.env", "sets a machine-wide environment variable", strings.Join(strings.Fields(l), " ")})
		}
		sh := &shell{cwd: x.Workdir}
		for _, seg := range Segments(c.Command) {
			hits = append(hits, classifyArgv(seg, x, sh))
		}
		for _, p := range c.Deletes {
			hits = append(hits, deleteRule(p, x, &shell{cwd: x.Workdir}))
		}
		if c.Kind == harness.ToolWrite || len(c.Deletes) > 0 {
			for _, p := range c.Paths {
				if systemDir(p, x.GOOS) {
					hits = append(hits, &Hit{"sys.dir", "writes into a system directory: " + p, normTarget(p, x, nil)})
				}
			}
		}
		if (c.Kind == harness.ToolOther || c.Kind == harness.ToolWrite || c.Kind == harness.ToolFetch) && mcpSend.MatchString(c.Name) {
			hits = append(hits, &Hit{"out.mcp", "tool " + c.Name + " looks like it sends a message", strings.TrimSpace(c.Name + " " + recipient(c.Input))})
		}
	}
	for _, e := range r.ExtraCommands {
		if re, err := regexp.Compile(e); err == nil {
			for _, seg := range Segments(c.Command) {
				if line := strings.Join(seg, " "); re.MatchString(line) {
					hits = append(hits, &Hit{"extra.command", "matches " + e, line})
				}
			}
		}
	}
	for _, e := range r.ExtraMCPTools {
		if re, err := regexp.Compile(e); err == nil && c.Name != "" && c.Kind == harness.ToolOther && re.MatchString(c.Name) {
			hits = append(hits, &Hit{"extra.mcp", "matches " + e, c.Name})
		}
	}
	for _, h := range hits {
		if h != nil && !r.disabled(h.Rule) {
			return h
		}
	}
	return nil
}

// recipient picks the addressee out of an MCP tool's input, if any.
func recipient(in any) string {
	m, _ := in.(map[string]any)
	for _, k := range []string{"to", "recipient", "recipients", "channel", "channel_id", "email", "user", "user_id", "chat_id", "url"} {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return strings.ToLower(v)
			}
		case []any:
			var parts []string
			for _, e := range v {
				if s, ok := e.(string); ok {
					parts = append(parts, strings.ToLower(s))
				}
			}
			if len(parts) > 0 {
				sort.Strings(parts)
				return strings.Join(parts, ",")
			}
		}
	}
	return ""
}

var mcpSend = regexp.MustCompile(`(?i)(send|post|reply|forward)[_-]?(message|mail|email|dm|chat|tweet)|chat[_.]?postmessage|gmail|outlook|slack_send|send_draft|webhook`)

// Segments splits a command line into simple commands (on ; && || | &,
// newlines, subshell parentheses and braces), lifts out command
// substitutions ($(...), `...`, <(...)), unwraps cmd /c, powershell
// -Command / -EncodedCommand, sh -c and launchers (sudo, env, npx, ...),
// and drops leading VAR=value assignments.
func Segments(cmd string) [][]string {
	var out [][]string
	outer, inner := substitutions(cmd)
	for _, part := range splitOps(outer) {
		argv := Fields(part)
		for {
			for len(argv) > 0 && strings.Contains(argv[0], "=") && !strings.HasPrefix(argv[0], "-") && !strings.ContainsAny(argv[0], `/\`) {
				argv = argv[1:]
			}
			if n := launcher(argv); n > 0 {
				argv = argv[n:]
				continue
			}
			break
		}
		if len(argv) == 0 {
			continue
		}
		if in, ok := unwrap(argv); ok {
			out = append(out, Segments(in)...)
			continue
		}
		out = append(out, argv)
	}
	for _, in := range inner {
		out = append(out, Segments(in)...)
	}
	return out
}

// launcher returns how many leading words of argv only launch the real
// command (sudo git push -f, env A=1 rm, npx rimraf, npm exec -- rimraf).
func launcher(argv []string) int {
	if len(argv) == 0 {
		return 0
	}
	n := 0
	switch e := exeName(argv[0]); {
	case oneOf(e, "sudo", "doas", "env", "nohup", "nice", "time", "command", "exec", "xargs", "npx", "pnpx", "bunx", "{", "}"):
		n = 1
	case oneOf(e, "npm", "pnpm", "yarn", "bun") && len(argv) > 1 && oneOf(argv[1], "exec", "dlx", "x"):
		n = 2
	default:
		return 0
	}
	for n < len(argv) && strings.HasPrefix(argv[n], "-") {
		if oneOf(argv[n], "-p", "--package", "-u", "--user", "-n") && n+1 < len(argv) {
			n++ // option with a value
		}
		n++
	}
	return n
}

// substitutions replaces every $(...), <(...), >(...) and `...` outside
// single quotes with a placeholder and returns the inner commands.
func substitutions(s string) (string, []string) {
	if !strings.ContainsAny(s, "$<>`") {
		return s, nil
	}
	var outer strings.Builder
	var inner []string
	rs := []rune(s)
	single := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'' && !single:
			single = true
		case c == '\'' && single:
			single = false
		case !single && (c == '$' || c == '<' || c == '>') && i+1 < len(rs) && rs[i+1] == '(':
			depth, j := 1, i+2
			for ; j < len(rs) && depth > 0; j++ {
				switch rs[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			end := j - 1
			if depth > 0 {
				end = len(rs)
			}
			inner = append(inner, string(rs[i+2:end]))
			outer.WriteString(" SUBST ")
			i = j - 1
			continue
		case !single && c == '`':
			if k := runeIndex(rs[i+1:], '`'); k >= 0 {
				inner = append(inner, string(rs[i+1:i+1+k]))
				outer.WriteString(" SUBST ")
				i += k + 1
				continue
			}
		}
		outer.WriteRune(c)
	}
	return outer.String(), inner
}

func runeIndex(rs []rune, r rune) int {
	for i, c := range rs {
		if c == r {
			return i
		}
	}
	return -1
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

// splitOps splits on unquoted ; && || | & ( ) and newlines, and on { }
// standing alone as words.
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
		case c == ';' || c == '\n' || c == '|' || c == '&' || c == '(' || c == ')':
			out = append(out, cur.String())
			cur.Reset()
			if i+1 < len(rs) && (rs[i+1] == c) {
				i++
			}
		case (c == '{' || c == '}') && (i == 0 || rs[i-1] == ' ' || rs[i-1] == '\t' || i+1 == len(rs) || rs[i+1] == ' ' || rs[i+1] == '\t'):
			out = append(out, cur.String()) // { ...; } groups, PowerShell script blocks
			cur.Reset()
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

func classifyArgv(argv []string, x Ctx, sh *shell) *Hit {
	exe := exeName(argv[0])
	args := argv[1:]
	low := make([]string, len(args))
	for i, a := range args {
		low[i] = strings.ToLower(a)
	}
	line := strings.ToLower(strings.Join(argv, " "))
	if h := urlRule(argv); h != nil {
		return h
	}
	if h := redirectRule(argv, x, sh); h != nil {
		return h
	}
	switch exe {
	case "cd", "chdir", "pushd", "set-location", "sl":
		sh.cd(positionals(args, x.GOOS), x)
		return nil
	case "git":
		return gitRule(args, x, sh)
	case "bfg":
		return &Hit{"git.rewrite", "bfg rewrites history", remoteTarget(x, sh.dir(x), "", "*")}
	case "rm", "del", "erase", "rd", "rmdir", "remove-item", "ri", "rimraf", "unlink", "shred", "trash", "rmtree":
		if regPath(line) {
			return &Hit{"sys.registry", "changes the registry", line}
		}
		for _, p := range args {
			if strings.HasPrefix(p, "-") || (strings.HasPrefix(p, "/") && len(p) == 2 && x.GOOS == "windows") {
				continue
			}
			if h := deleteRule(p, x, sh); h != nil {
				return h
			}
		}
	case "reg":
		if len(low) > 0 && oneOf(low[0], "add", "delete", "import", "restore", "load", "unload", "copy") {
			return &Hit{"sys.registry", "changes the registry", line}
		}
	case "regedit", "regedt32", "regini":
		if len(args) > 0 {
			return &Hit{"sys.registry", exe + " imports into the registry", line}
		}
	case "set-itemproperty", "new-itemproperty", "remove-itemproperty", "new-item", "set-item", "rename-itemproperty",
		"copy-item", "move-item", "rename-item", "clear-item", "clear-itemproperty", "sp", "ni", "si":
		if regPath(line) {
			return &Hit{"sys.registry", "changes the registry", line}
		}
	case "sc":
		if len(low) > 0 && oneOf(low[0], "create", "delete", "config", "stop", "start", "failure") {
			return &Hit{"sys.service", "changes a Windows service", line}
		}
	case "new-service", "set-service", "remove-service", "stop-service", "start-service", "restart-service":
		return &Hit{"sys.service", "changes a Windows service", line}
	case "schtasks":
		if containsAny(low, "/create", "/delete", "/change") {
			return &Hit{"sys.task", "changes a scheduled task", line}
		}
	case "register-scheduledtask", "unregister-scheduledtask", "set-scheduledtask":
		return &Hit{"sys.task", "changes a scheduled task", line}
	case "setx":
		if containsAny(low, "/m") {
			return &Hit{"sys.env", "sets a machine-wide environment variable", line}
		}
	case "msiexec", "bcdedit", "add-appxpackage", "add-appxprovisionedpackage", "remove-appxpackage",
		"remove-appxprovisionedpackage", "add-appxvolume", "install-package", "uninstall-package", "pkgmgr":
		return &Hit{"sys.installer", exe + " changes installed software", line}
	case "dism":
		if strings.Contains(line, "/online") {
			return &Hit{"sys.installer", "dism changes the running system", line}
		}
	case "winget", "choco", "apt", "apt-get", "dnf", "yum", "scoop":
		if len(low) > 0 && oneOf(low[0], "install", "uninstall", "upgrade", "remove", "purge") {
			return &Hit{"sys.installer", exe + " " + low[0], line}
		}
	case "send-mailmessage", "sendmail", "mail", "mutt", "mailx", "msmtp", "swaks":
		return &Hit{"out.mail", "sends email", mailTarget(args)}
	case "start", "start-process", "saps", "invoke-item", "ii":
		for _, p := range positionals(args, x.GOOS) {
			if installer(p) {
				return &Hit{"sys.installer", "runs an installer", strings.ToLower(p)}
			}
		}
	}
	if dsts := writeTargets(exe, args, x.GOOS); dsts != nil {
		for _, p := range dsts {
			if sysPath(p, x, sh) {
				return &Hit{"sys.dir", "writes into a system directory: " + p, normTarget(p, x, sh)}
			}
		}
	}
	if installer(argv[0]) {
		return &Hit{"sys.installer", "runs an installer", strings.ToLower(argv[0])}
	}
	return nil
}

// installer reports an installer file: *.msi/*.msix/*.appx, or an .exe
// whose name says setup or install (MyInstaller.exe, setup-x64.exe).
func installer(p string) bool {
	l := strings.ToLower(strings.Trim(p, `"'`))
	for _, ext := range []string{".msi", ".msix", ".msixbundle", ".appx", ".appxbundle", ".msp"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	base := exeName(l)
	return strings.HasSuffix(l, ".exe") && (strings.Contains(base, "setup") || strings.Contains(base, "install"))
}

func regPath(line string) bool {
	return strings.Contains(line, "hklm:") || strings.Contains(line, "hkcu:") || strings.Contains(line, "registry::") ||
		strings.Contains(line, "hkey_local_machine") || strings.Contains(line, "hkey_current_user")
}

// positionals drops option words (and Windows /x switches).
func positionals(args []string, goos string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || (goos == "windows" && strings.HasPrefix(a, "/") && len(a) <= 3) || a == "SUBST" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// writeTargets returns the paths a file-writing command writes to (nil
// when exe does not write files).
func writeTargets(exe string, args []string, goos string) []string {
	pos := positionals(args, goos)
	dest := ""
	for i, a := range args {
		l := strings.ToLower(a)
		if (strings.HasPrefix(l, "-dest") || l == "-t" || l == "--target-directory") && i+1 < len(args) {
			dest = args[i+1]
		} else if strings.HasPrefix(l, "--target-directory=") {
			dest = a[len("--target-directory="):]
		}
	}
	switch exe {
	case "cp", "mv", "copy", "move", "xcopy", "ln", "install", "rsync", "scp", "copy-item", "cpi", "move-item", "mi",
		"rename-item", "rni", "ren", "rename":
		if dest != "" {
			return []string{dest}
		}
		if len(pos) > 0 {
			return pos[len(pos)-1:]
		}
		return []string{}
	case "robocopy":
		if len(pos) > 1 {
			return pos[1:2]
		}
		return []string{}
	case "tee", "touch", "mkdir", "md", "new-item", "ni", "set-content", "add-content", "out-file", "chmod", "chown",
		"icacls", "takeown", "attrib", "truncate", "export-csv", "expand-archive":
		return append(pos, dest)
	case "sed", "perl": // in-place edits only
		for _, a := range args {
			if strings.HasPrefix(a, "-i") || a == "--in-place" {
				return pos
			}
		}
	case "dd":
		var out []string
		for _, a := range args {
			if strings.HasPrefix(a, "of=") {
				out = append(out, a[3:])
			}
		}
		return out
	}
	return nil
}

// redirectRule flags > / >> redirections into system directories.
func redirectRule(argv []string, x Ctx, sh *shell) *Hit {
	for i, a := range argv {
		k := strings.IndexByte(a, '>')
		if k < 0 {
			continue
		}
		t := strings.TrimLeft(a[k:], ">")
		if t == "" && i+1 < len(argv) {
			t = argv[i+1]
		}
		if t == "" || strings.HasPrefix(t, "&") || strings.EqualFold(t, "nul") || t == "/dev/null" || t == "$null" {
			continue
		}
		if sysPath(t, x, sh) {
			return &Hit{"sys.dir", "writes into a system directory: " + t, normTarget(t, x, sh)}
		}
	}
	return nil
}

func deleteRule(p string, x Ctx, sh *shell) *Hit {
	if outside(p, x, sh) {
		return &Hit{"fs.delete_outside", "deletes outside the workdir: " + p, normTarget(p, x, sh)}
	}
	if sysPath(p, x, sh) {
		return &Hit{"sys.dir", "deletes in a system directory: " + p, normTarget(p, x, sh)}
	}
	return nil
}

func gitRule(args []string, x Ctx, sh *shell) *Hit {
	dir := sh.dir(x)
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") { // global options
		switch args[i] {
		case "-C":
			if i+1 < len(args) && dir != "" {
				dir = join(dir, args[i+1], x.GOOS)
			}
			i++
		case "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--exec-path", "--list-cmds":
			i++ // option with a separate value
		}
		i++
	}
	if i >= len(args) {
		return nil
	}
	sub, rest := args[i], args[i+1:]
	pushed := func() bool {
		if x.HeadPushed != nil {
			return x.HeadPushed(dir)
		}
		if x.Git == nil || dir == "" {
			return false
		}
		_, ok := x.Git(dir, "merge-base", "--is-ancestor", "HEAD", "@{u}")
		return ok
	}
	switch sub {
	case "push":
		var pos []string
		why, all := "", false
		for k := 0; k < len(rest); k++ {
			a := rest[k]
			switch {
			case a == "-o" || a == "--push-option" || a == "--repo" || a == "--receive-pack" || a == "--exec":
				k++
			case a == "-f" || a == "--force" || strings.HasPrefix(a, "--force-with-lease") || a == "--force-if-includes" ||
				a == "--delete" || a == "-d" || a == "--prune":
				why = firstNonEmpty(why, "git push "+a)
			case a == "--mirror" || a == "--all" || a == "--tags":
				all = true
				if a == "--mirror" {
					why = firstNonEmpty(why, "git push "+a)
				}
			case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "f"):
				why = firstNonEmpty(why, "git push "+a)
			case !strings.HasPrefix(a, "-"):
				pos = append(pos, a)
				if len(pos) >= 2 && (strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":")) {
					why = firstNonEmpty(why, "git push refspec "+a)
				}
			}
		}
		if why == "" {
			return nil
		}
		remote, branches := "", []string{}
		if len(pos) > 0 {
			remote = pos[0]
		}
		for _, r := range pos[min(1, len(pos)):] {
			r = strings.TrimPrefix(r, "+")
			if k := strings.LastIndexByte(r, ':'); k >= 0 {
				r = r[k+1:]
			}
			branches = append(branches, strings.TrimPrefix(r, "refs/heads/"))
		}
		branch := strings.Join(branches, ",")
		if all {
			branch = "*"
		}
		return &Hit{"git.force", why, remoteTarget(x, dir, remote, branch)}
	case "filter-branch", "filter-repo":
		return &Hit{"git.rewrite", "git " + sub + " rewrites history", remoteTarget(x, dir, "", "*")}
	case "rebase", "reset":
		if !(sub == "rebase" && containsAny(rest, "--abort", "--continue", "--skip", "--quit")) && !(sub == "reset" && len(rest) == 0) && pushed() {
			return &Hit{"git.rewrite", "git " + sub + " on a pushed branch", remoteTarget(x, dir, "", "")}
		}
	case "commit":
		if containsAny(rest, "--amend") && pushed() {
			return &Hit{"git.rewrite", "git commit --amend on a pushed branch", remoteTarget(x, dir, "", "")}
		}
	}
	return nil
}

// remoteTarget names a git target as "<normalized remote URL>#<branch>".
// An empty remote means the branch's upstream remote (else origin); an
// empty branch means the upstream branch (else the current branch).
func remoteTarget(x Ctx, dir, remote, branch string) string {
	git := func(args ...string) string {
		if x.Git == nil || dir == "" {
			return ""
		}
		out, ok := x.Git(dir, args...)
		if !ok {
			return ""
		}
		return out
	}
	if remote == "" || branch == "" {
		if up := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); up != "" {
			if k := strings.IndexByte(up, '/'); k > 0 {
				remote = firstNonEmpty(remote, up[:k])
				branch = firstNonEmpty(branch, up[k+1:])
			}
		}
		remote = firstNonEmpty(remote, "origin")
		if branch == "" {
			branch = git("rev-parse", "--abbrev-ref", "HEAD")
		}
	}
	url := remote
	if !strings.Contains(remote, "/") && !strings.Contains(remote, ":") {
		url = firstNonEmpty(git("remote", "get-url", remote), dir+"|"+remote)
	}
	return normRemote(url) + "#" + branch
}

// normRemote maps the URL forms of one repository to one string:
// git@host:o/r.git, ssh://git@host/o/r, https://user@host/o/r/ -> host/o/r.
func normRemote(u string) string {
	l := strings.TrimSpace(u)
	if k := strings.Index(l, "://"); k >= 0 {
		l = l[k+3:]
	} else if at := strings.IndexByte(l, '@'); at >= 0 && strings.Contains(l[at:], ":") {
		l = strings.Replace(l[at+1:], ":", "/", 1)
	}
	if at := strings.IndexByte(l, '@'); at >= 0 && at < strings.IndexByte(l+"/", '/') {
		l = l[at+1:]
	}
	l = strings.TrimSuffix(strings.TrimSuffix(l, "/"), ".git")
	if k := strings.IndexByte(l, '/'); k > 0 {
		l = strings.ToLower(l[:k]) + l[k:]
	}
	return l
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func mailTarget(args []string) string {
	seen := map[string]bool{}
	var to []string
	add := func(a string) {
		a = strings.ToLower(strings.Trim(a, `"'<>,`))
		if a != "" && !seen[a] {
			seen[a] = true
			to = append(to, a)
		}
	}
	for i, a := range args {
		l := strings.ToLower(a)
		if (l == "-to" || l == "-t") && i+1 < len(args) {
			add(args[i+1])
		} else if strings.Contains(a, "@") && !strings.HasPrefix(a, "-") {
			add(a)
		}
	}
	if len(to) == 0 {
		return "mail"
	}
	sort.Strings(to)
	return strings.Join(to, ",")
}

var urlRe = regexp.MustCompile(`(?i)\b(https?|wss?)://[^\s"'<>\x60)]+`)

// urlRule flags requests to webhooks and to messaging / mail APIs in any
// word of the command (curl -X POST .../api/chat.postMessage, a python -c
// one-liner with requests.post(...)).
func urlRule(argv []string) *Hit {
	for _, a := range argv {
		if !strings.Contains(a, "://") {
			continue
		}
		for _, u := range urlRe.FindAllString(a, -1) {
			if webhook(u) {
				return &Hit{"out.webhook", "posts to a webhook: " + hostOf(u), endpoint(u)}
			}
			if outboundAPI(u) {
				return &Hit{"out.api", "calls a messaging or mail API: " + endpoint(u), endpoint(u)}
			}
		}
	}
	return nil
}

var outboundPath = regexp.MustCompile(`(?i)(chat\.(postmessage|postephemeral|schedulemessage|memessage|update)|files\.(upload|completeuploadexternal)|conversations\.(invite|open)|/send[-_]?(message|mail|email|sms|photo|document)?(/|$)|/sendmessage|/sendmail|/messages/send|/mail/send|/v3/mail|/messages(\.json)?$|/channels/[0-9]+/messages)`)

var outboundHosts = regexp.MustCompile(`(?i)(^|\.)(slack\.com|discord(app)?\.com|api\.telegram\.org|graph\.microsoft\.com|gmail\.googleapis\.com|api\.sendgrid\.com|api\.mailgun\.net|api\.postmarkapp\.com|api\.twilio\.com|api\.pushover\.net|ntfy\.sh|api\.line\.me|qyapi\.weixin\.qq\.com|oapi\.dingtalk\.com|open\.feishu\.cn|open\.larksuite\.com)$`)

var outboundAnyHost = regexp.MustCompile(`(?i)(chat\.postmessage|/sendmessage|/sendmail|/messages/send|/mail/send)`)

func outboundAPI(u string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	if outboundHosts.MatchString(pu.Hostname()) {
		return pu.Path != "" && pu.Path != "/" && outboundPath.MatchString(pu.Path) ||
			strings.HasSuffix(strings.ToLower(pu.Hostname()), "ntfy.sh")
	}
	return outboundAnyHost.MatchString(pu.Path)
}

// endpoint is a URL's host and path (no query, no credentials).
func endpoint(u string) string {
	pu, err := url.Parse(strings.Trim(u, `"'`))
	if err != nil {
		return u
	}
	return strings.ToLower(pu.Hostname()) + strings.TrimSuffix(pu.Path, "/")
}

var unexpanded = regexp.MustCompile(`^~|\$|%[A-Za-z_]+%`)

// norm cleans a path with the target OS's rules (on any host): on Windows
// it is case-insensitive and uses forward slashes.
func norm(s, goos string) string {
	if goos == "windows" {
		s = strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
	}
	return path.Clean(s)
}

func isAbs(a, goos string) bool {
	return strings.HasPrefix(a, "/") || (goos == "windows" && len(a) > 1 && a[1] == ':')
}

func join(dir, p, goos string) string {
	if unexpanded.MatchString(p) {
		return ""
	}
	a := norm(p, goos)
	if isAbs(a, goos) {
		return a
	}
	if dir == "" {
		return ""
	}
	return path.Join(norm(dir, goos), a)
}

// dir is the shell's current directory ("" if unknown).
func (sh *shell) dir(x Ctx) string {
	if sh == nil {
		return x.Workdir
	}
	return sh.cwd
}

func (sh *shell) cd(args []string, x Ctx) {
	if len(args) == 0 {
		sh.cwd = "" // home
		return
	}
	sh.cwd = join(sh.cwd, args[len(args)-1], x.GOOS)
}

// resolve returns the absolute, normalized form of p ("" if unknown).
func resolve(p string, x Ctx, sh *shell) string {
	p = strings.Trim(p, `"'`)
	if unexpanded.MatchString(p) {
		return ""
	}
	return join(sh.dir(x), p, x.GOOS)
}

func outside(p string, x Ctx, sh *shell) bool {
	a := resolve(p, x, sh)
	if a == "" {
		return true // cannot tell where it points
	}
	root := norm(x.Workdir, x.GOOS)
	return a != root && !strings.HasPrefix(a, strings.TrimSuffix(root, "/")+"/")
}

// normTarget is the grant target of a path: absolute and normalized, or the
// raw text when it cannot be resolved.
func normTarget(p string, x Ctx, sh *shell) string {
	if a := resolve(p, x, sh); a != "" {
		return a
	}
	return p
}

func sysPath(p string, x Ctx, sh *shell) bool {
	if systemDir(p, x.GOOS) {
		return true
	}
	a := resolve(p, x, sh)
	if a == "" {
		return false
	}
	if x.GOOS == "windows" {
		return systemDir(strings.ReplaceAll(a, "/", `\`), x.GOOS)
	}
	return systemDir(a, x.GOOS)
}

func systemDir(p, goos string) bool {
	p = strings.Trim(p, `"'`)
	l := strings.ToLower(strings.ReplaceAll(p, "/", `\`))
	if goos == "windows" {
		for _, d := range []string{`c:\windows`, `c:\program files`, `c:\programdata`, `%windir%`, `%systemroot%`, `%programfiles%`,
			`%programfiles(x86)%`, `%programdata%`, `$env:windir`, `$env:systemroot`, `$env:programfiles`, `$env:programdata`} {
			if l == d || strings.HasPrefix(l, d+`\`) || strings.HasPrefix(l, d+` (x86)`) {
				return true
			}
		}
		return false
	}
	for _, d := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/lib", "/lib64", "/system", "/opt", "/var/lib", "/Library", "/System"} {
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
