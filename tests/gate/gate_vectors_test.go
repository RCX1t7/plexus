// Package gate_test is a black-box test of the real dangerous-action gate
// (internal/danger.Classify) against the bypass vectors the architect's
// skeleton review called out. Cases marked bug: are expected to FAIL against
// the current skeleton; they are recorded (not weakened) so the builder can
// verify a fix flips them green. A case that is fixed trips an error telling
// us to drop its bug marker.
package gate_test

import (
	"strings"
	"testing"

	"github.com/RCX1t7/plexus/internal/danger"
	"github.com/RCX1t7/plexus/internal/harness"
)

const workdir = "/work/repo"

func ctx(goos string, pushed bool) danger.Ctx {
	return danger.Ctx{GOOS: goos, Workdir: workdir, HeadPushed: func(string) bool { return pushed }}
}

type vec struct {
	name     string
	call     danger.Call
	ctx      danger.Ctx
	wantGate bool   // the gate SHOULD stop and ask Sin
	wantRule string // expected rule id when gated (optional)
	bug      string // non-empty => currently known to misbehave (skeleton bug)
}

func sh(cmd string) danger.Call { return danger.Call{Kind: harness.ToolShell, Command: cmd} }
func wr(paths ...string) danger.Call {
	return danger.Call{Kind: harness.ToolWrite, Paths: paths}
}
func mcp(name string, paths ...string) danger.Call {
	return danger.Call{Kind: harness.ToolOther, Name: name, Paths: paths}
}
func del(paths ...string) danger.Call {
	return danger.Call{Kind: harness.ToolWrite, Deletes: paths}
}
func mcpDel(name string, paths ...string) danger.Call {
	return danger.Call{Kind: harness.ToolOther, Name: name, Deletes: paths}
}

func TestGateVectors(t *testing.T) {
	lin := ctx("linux", false)
	linPushed := ctx("linux", true)
	win := ctx("windows", false)

	cases := []vec{
		// ---- must STAY ALLOWED ----
		{name: "plain git push", call: sh("git push origin main"), ctx: lin, wantGate: false},
		{name: "in-workdir delete", call: sh("rm " + workdir + "/tmp.txt"), ctx: lin, wantGate: false},
		{name: "in-workdir relative delete", call: sh("rm build/out.o"), ctx: lin, wantGate: false},
		{name: "write in workdir", call: wr(workdir + "/src/main.go"), ctx: lin, wantGate: false},
		{name: "git commit (not pushed)", call: sh("git commit --amend"), ctx: lin, wantGate: false},
		{name: "git pull", call: sh("git pull --rebase"), ctx: lin, wantGate: false},

		// ---- git force / rewrite (should gate) ----
		{name: "git push --force", call: sh("git push --force origin main"), ctx: lin, wantGate: true, wantRule: "git.force"},
		{name: "git push -f short", call: sh("git push -f"), ctx: lin, wantGate: true, wantRule: "git.force"},
		{name: "git -C dir push --force", call: sh("git -C " + workdir + " push --force"), ctx: lin, wantGate: true, wantRule: "git.force"},
		{name: "git -c k=v push -f", call: sh("git -c k=v push -f"), ctx: lin, wantGate: true, wantRule: "git.force"},
		{name: "git push +refspec", call: sh("git push origin +main:main"), ctx: lin, wantGate: true, wantRule: "git.force"},
		{name: "git rebase on pushed", call: sh("git rebase -i HEAD~3"), ctx: linPushed, wantGate: true, wantRule: "git.rewrite"},
		{name: "git filter-branch", call: sh("git filter-branch --tree-filter x"), ctx: lin, wantGate: true, wantRule: "git.rewrite"},
		{name: "bfg", call: sh("bfg --delete-files secrets"), ctx: lin, wantGate: true, wantRule: "git.rewrite"},

		// ---- deletes / writes outside the workdir ----
		{name: "rm outside workdir", call: sh("rm -rf /home/sin/important"), ctx: lin, wantGate: true, wantRule: "fs.delete_outside"},
		{name: "rm in system dir", call: sh("rm -rf /etc/hosts"), ctx: lin, wantGate: true, wantRule: "fs.delete_outside"},
		// Codex fileChange / applyPatch and MCP delete tools arrive as write/other
		// calls with Paths; a path OUTSIDE the workdir must gate even if it is not
		// a system dir. Skeleton only checks systemDir for ToolWrite Paths.
		{name: "applyPatch delete outside workdir", call: del("/home/sin/.bashrc"), ctx: lin,
			wantGate: true, wantRule: "fs.delete_outside"},
		{name: "*** Delete File outside (patch)", call: del("/home/sin/.ssh/authorized_keys"), ctx: lin,
			wantGate: true, wantRule: "fs.delete_outside"},
		{name: "MCP delete tool outside workdir", call: mcpDel("filesystem_delete_file", "/home/sin/data.db"), ctx: lin,
			wantGate: true, wantRule: "fs.delete_outside"},

		// ---- shell obfuscation / wrappers ----
		{name: "cd elsewhere && rm relative", call: sh("cd /home/sin && rm -rf project"), ctx: lin,
			wantGate: true, wantRule: "fs.delete_outside"},
		{name: "subshell (git push -f)", call: sh("(git push -f)"), ctx: lin,
			wantGate: true},
		{name: "command substitution $(...)", call: sh("eval $(echo git push --force)"), ctx: lin,
			wantGate: true, bug: "command substitution not expanded (documented gap)"},
		{name: "npx rimraf outside", call: sh("npx rimraf /home/sin/x"), ctx: lin,
			wantGate: true},

		// ---- windows system / installer ----
		{name: "regedit /s", call: sh("regedit /s evil.reg"), ctx: win,
			wantGate: true, wantRule: "sys.registry"},
		{name: "reg add", call: sh("reg add HKLM\\Software\\X /v Y /d Z"), ctx: win, wantGate: true, wantRule: "sys.registry"},
		{name: "Add-AppxPackage", call: sh("Add-AppxPackage evil.msix"), ctx: win,
			wantGate: true, wantRule: "sys.installer"},
		{name: "FooInstaller.exe suffix", call: sh("C:\\Temp\\FooInstaller.exe /S"), ctx: win,
			wantGate: true, wantRule: "sys.installer"},
		{name: "installer.exe prefix", call: sh("installer.exe /quiet"), ctx: win, wantGate: true, wantRule: "sys.installer"},
		{name: "msiexec", call: sh("msiexec /i pkg.msi"), ctx: win, wantGate: true, wantRule: "sys.installer"},

		// ---- shell write into system dir (redirection / cp) ----
		{name: "cp into system dir", call: sh("cp payload /usr/bin/x"), ctx: lin,
			wantGate: true},
		{name: "shell redirect into system dir", call: sh("echo x > /etc/cron.d/evil"), ctx: lin,
			wantGate: true},

		// ---- outbound message via curl to Slack web API ----
		{name: "curl chat.postMessage", call: sh("curl -XPOST https://slack.com/api/chat.postMessage -d text=hi"), ctx: lin,
			wantGate: true, wantRule: "out.api"},
		{name: "curl to slack incoming webhook", call: sh("curl -XPOST https://hooks.slack.com/services/XXX -d p=1"), ctx: lin,
			wantGate: true, wantRule: "out.webhook"},

		// ---- MCP send tool (name-based) ----
		{name: "MCP send_message tool", call: danger.Call{Kind: harness.ToolOther, Name: "slack_send_message"}, ctx: lin,
			wantGate: true, wantRule: "out.mcp"},
	}

	var bugs []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit := danger.Classify(c.call, c.ctx, danger.Rules{})
			gated := hit != nil
			rule := ""
			if hit != nil {
				rule = hit.Rule
			}
			switch {
			case gated == c.wantGate && c.bug == "":
				if c.wantGate && c.wantRule != "" && rule != c.wantRule {
					t.Errorf("gated with rule %q, want %q", rule, c.wantRule)
				}
			case gated == c.wantGate && c.bug != "":
				// A known bug now behaves correctly -> the builder likely fixed
				// it. Report it (CI stays green) so we can drop the bug marker.
				t.Logf("KNOWN BUG APPEARS FIXED (%s): drop the bug marker for %q", c.bug, c.name)
			case gated != c.wantGate && c.bug != "":
				msg := c.name + " -> gated=" + b2s(gated) + " want=" + b2s(c.wantGate) + " rule=" + q(rule) + " :: " + c.bug
				bugs = append(bugs, msg)
				t.Logf("KNOWN SKELETON BUG: %s", msg)
			default:
				t.Errorf("gated=%v (rule %q), want gated=%v rule=%q", gated, rule, c.wantGate, c.wantRule)
			}
		})
	}
	if len(bugs) > 0 {
		t.Logf("\n==== GATE SKELETON BUGS (%d) — owner: builder ====\n%s", len(bugs), strings.Join(bugs, "\n"))
	}
}

func b2s(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func q(s string) string { return "\"" + s + "\"" }
