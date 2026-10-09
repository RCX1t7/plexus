package danger

import (
	"testing"

	"github.com/RCX1t7/plexus/internal/harness"
)

// Review items 3 and 4: each miss is a positive vector with a negative twin.
func TestClassifierMisses(t *testing.T) {
	lin := Ctx{GOOS: "linux", Workdir: "/home/u/work"}
	win := Ctx{GOOS: "windows", Workdir: `C:\Users\u\work`}
	sh := func(cmd string) Call { return Call{Kind: harness.ToolShell, Command: cmd} }
	cases := []struct {
		name string
		c    Call
		x    Ctx
		want string
	}{
		{"cd / then rm", sh("cd / && rm -rf etc/x"), lin, "fs.delete_outside"},
		{"cd inside then rm", sh("cd sub && rm -rf build"), lin, ""},
		{"cd .. then rm", sh("cd .. ; rm -rf other"), lin, "fs.delete_outside"},
		{"cd /d windows", sh(`cd /d C:\ && del /s /q temp`), win, "fs.delete_outside"},
		{"subshell", sh("(git push -f)"), lin, "git.force"},
		{"subshell plain", sh("(git push)"), lin, ""},
		{"command subst", sh("echo $(git push -f)"), lin, "git.force"},
		{"quoted subst", sh(`echo "$(git push --force origin main)"`), lin, "git.force"},
		{"backticks", sh("echo `git push -f`"), lin, "git.force"},
		{"subst plain", sh("echo $(git rev-parse HEAD)"), lin, ""},
		{"brace group", sh("{ git push -f; }"), lin, "git.force"},
		{"--namespace", sh("git --namespace x push -f"), lin, "git.force"},
		{"--namespace plain", sh("git --namespace x push origin main"), lin, ""},
		{"npx rimraf", sh("npx rimraf ../x"), lin, "fs.delete_outside"},
		{"npx rimraf inside", sh("npx rimraf dist"), lin, ""},
		{"npm exec rimraf", sh("npm exec -- rimraf ../x"), lin, "fs.delete_outside"},
		{"regedit /s", sh(`regedit /s x.reg`), win, "sys.registry"},
		{"regedit bare", sh(`regedit`), win, ""},
		{"Add-AppxPackage", sh(`Add-AppxPackage .\app.msix`), win, "sys.installer"},
		{"Get-AppxPackage", sh(`Get-AppxPackage *x*`), win, ""},
		{"MyInstaller.exe", sh(`.\MyInstaller.exe /quiet`), win, "sys.installer"},
		{"Start-Process setup", sh(`Start-Process .\setup-x64.exe`), win, "sys.installer"},
		{"plain exe", sh(`.\mytool.exe --help`), win, ""},
		{"Copy-Item to System32", sh(`Copy-Item .\x.dll C:\Windows\System32`), win, "sys.dir"},
		{"Copy-Item -Destination", sh(`Copy-Item -Path x.dll -Destination C:\Windows\System32\x.dll`), win, "sys.dir"},
		{"Copy-Item inside", sh(`Copy-Item .\a.txt .\b.txt`), win, ""},
		{"append /etc/hosts", sh("echo 1.2.3.4 x >> /etc/hosts"), lin, "sys.dir"},
		{"tee /etc/hosts", sh("echo x | sudo tee -a /etc/hosts"), lin, "sys.dir"},
		{"redirect inside", sh("echo x > out.txt 2>&1"), lin, ""},
		{"cp from /etc", sh("cp /etc/hosts ./hosts.bak"), lin, ""},
		{"curl postMessage", sh(`curl -X POST https://slack.com/api/chat.postMessage -H "Authorization: Bearer $T" -d '{}'`), lin, "out.api"},
		{"curl slack read", sh(`curl https://slack.com/api/conversations.history`), lin, ""},
		{"python requests", sh(`python -c "import requests; requests.post('https://api.telegram.org/bot1/sendMessage', data={})"`), lin, "out.webhook"},
		{"sendgrid", sh(`curl https://api.sendgrid.com/v3/mail/send -d @m.json`), lin, "out.api"},
		{"graph sendMail", sh(`Invoke-RestMethod -Method Post -Uri https://graph.microsoft.com/v1.0/me/sendMail`), win, "out.api"},
		{"plain http", sh(`curl https://api.github.com/repos/o/r`), lin, ""},
	}
	for _, c := range cases {
		h := Classify(c.c, c.x, Rules{})
		got := ""
		if h != nil {
			got = h.Rule
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q (%v)", c.name, got, c.want, h)
		}
	}
}

// Review item 3: deletes outside the workdir through patches, Codex
// fileChange and MCP delete tools.
func TestDeletePaths(t *testing.T) {
	lin := Ctx{GOOS: "linux", Workdir: "/home/u/work"}
	call := func(r harness.ToolRequest) Call {
		r = harness.Normalize(r)
		return Call{Kind: r.Kind, Name: r.Name, Command: r.Command, Paths: r.Paths, Deletes: r.Deletes, Input: r.Input}
	}
	patch := func(body string) harness.ToolRequest {
		return harness.ToolRequest{Name: "apply_patch", Input: map[string]any{"input": "*** Begin Patch\n" + body + "*** End Patch\n"}}
	}
	cases := []struct {
		name string
		c    Call
		want string
	}{
		{"patch delete outside", call(patch("*** Delete File: ../other/x.go\n")), "fs.delete_outside"},
		{"patch delete inside", call(patch("*** Delete File: old.go\n")), ""},
		{"patch move away from outside", call(patch("*** Update File: /home/u/elsewhere/a.go\n*** Move to: a.go\n@@\n")), "fs.delete_outside"},
		{"patch update outside (a write)", call(patch("*** Update File: ../other/x.go\n@@\n")), ""},
		{"patch add sysdir", call(patch("*** Add File: /etc/cron.d/x\n+x\n")), "sys.dir"},
		{"shell heredoc patch", call(harness.ToolRequest{Name: "Bash", Kind: harness.ToolShell,
			Command: "apply_patch <<'EOF'\n*** Begin Patch\n*** Delete File: /home/u/other/y\n*** End Patch\nEOF"}), "fs.delete_outside"},
		{"codex fileChange delete", Call{Kind: harness.ToolWrite, Name: "fileChange", Paths: []string{"../x"}, Deletes: []string{"../x"}}, "fs.delete_outside"},
		{"codex fileChange edit", Call{Kind: harness.ToolWrite, Name: "fileChange", Paths: []string{"../x"}}, ""},
		{"codex fileChange unknown item", Call{Kind: harness.ToolWrite, Name: "fileChange", Input: map[string]any{"itemId": "never-started"}}, "exec.opaque"},
		{"write tool without paths (not fileChange)", Call{Kind: harness.ToolWrite, Name: "mcp__notion__create_page"}, ""},
		{"mcp delete outside", call(harness.ToolRequest{Name: "mcp__fs__delete_file", Kind: harness.ToolOther, Input: map[string]any{"path": "/home/u/other"}}), "fs.delete_outside"},
		{"mcp delete inside", call(harness.ToolRequest{Name: "mcp__fs__delete_file", Kind: harness.ToolOther, Input: map[string]any{"path": "tmp/a"}}), ""},
		{"mcp rm outside", call(harness.ToolRequest{Name: "fs_rm", Input: map[string]any{"paths": []any{"/tmp/x"}}}), "fs.delete_outside"},
		{"mcp read outside", call(harness.ToolRequest{Name: "mcp__fs__read_file", Kind: harness.ToolOther, Input: map[string]any{"path": "/home/u/other"}}), ""},
	}
	for _, c := range cases {
		h := Classify(c.c, lin, Rules{})
		got := ""
		if h != nil {
			got = h.Rule
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q (%v; %+v)", c.name, got, c.want, h, c.c)
		}
	}
}

func TestTargets(t *testing.T) {
	git := func(dir string, args ...string) (string, bool) {
		key := dir + " " + join2(args)
		out, ok := map[string]string{
			"/w remote get-url origin":                                "git@github.com:Owner/Repo.git",
			"/w remote get-url up":                                    "https://user@GitHub.com/Owner/Repo/",
			"/w rev-parse --abbrev-ref --symbolic-full-name @{u}":     "origin/main",
			"/w/sub remote get-url origin":                            "https://github.com/o/sub.git",
			"/w/sub rev-parse --abbrev-ref --symbolic-full-name @{u}": "origin/dev",
		}[key]
		return out, ok
	}
	x := Ctx{GOOS: "linux", Workdir: "/w", Git: git, HeadPushed: func(string) bool { return true }}
	sh := func(cmd string) Call { return Call{Kind: harness.ToolShell, Command: cmd} }
	for cmd, want := range map[string]string{
		"git push -f origin main":                         "github.com/Owner/Repo#main",
		"git push --force up main":                        "github.com/Owner/Repo#main",
		"git push -f":                                     "github.com/Owner/Repo#main",
		"git push origin +feature:release":                "github.com/Owner/Repo#release",
		"git -C sub push -f":                              "github.com/o/sub#dev",
		"cd sub && git push -f":                           "github.com/o/sub#dev",
		"git push --mirror":                               "github.com/Owner/Repo#*",
		"git rebase -i HEAD~2":                            "github.com/Owner/Repo#main",
		"rm -rf ../other/x":                               "/other/x",
		"curl https://hooks.slack.com/services/T/B/X?a=1": "hooks.slack.com/services/T/B/X",
		"Send-MailMessage -To B@x.org -Subject s":         "b@x.org",
	} {
		h := Classify(sh(cmd), x, Rules{})
		if h == nil || h.Target != want {
			t.Errorf("%s: %+v want target %q", cmd, h, want)
		}
	}
	if h := Classify(Call{Kind: harness.ToolOther, Name: "mcp__slack__send_message", Input: map[string]any{"channel": "C123"}}, x, Rules{}); h == nil || h.Target != "mcp__slack__send_message c123" {
		t.Errorf("mcp target %+v", h)
	}
}

func join2(a []string) string {
	s := ""
	for i, x := range a {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}

// A call with its own working directory (DSH bash workdir, MCP shell cwd):
// relative paths resolve against it, falling back to Ctx.Workdir.
func TestCallWorkdir(t *testing.T) {
	lin := Ctx{GOOS: "linux", Workdir: "/home/u/work"}
	win := Ctx{GOOS: "windows", Workdir: `C:\Users\u\work`}
	call := func(r harness.ToolRequest) Call {
		r = harness.Normalize(r)
		return Call{Kind: r.Kind, Name: r.Name, Command: r.Command, Paths: r.Paths, Deletes: r.Deletes, Input: r.Input, Workdir: r.Workdir}
	}
	bash := func(cmd, wd string) Call {
		return call(harness.ToolRequest{Name: "bash", Input: map[string]any{"command": cmd, "workdir": wd}})
	}
	cases := []struct {
		name string
		c    Call
		x    Ctx
		want string
	}{
		{"relative rm in outside workdir", bash("rm -rf build", "/home/u/other"), lin, "fs.delete_outside"},
		{"relative rm in sub workdir", bash("rm -rf build", "sub"), lin, ""},
		{"relative rm in parent workdir", bash("rm -rf work2", ".."), lin, "fs.delete_outside"},
		{"relative rm, no call workdir", bash("rm -rf build", ""), lin, ""},
		{"unexpanded call workdir fails closed", bash("rm -rf build", "~/x"), lin, "fs.delete_outside"},
		{"cd inside call workdir", bash("cd .. && rm -rf work/x", "/home/u/work/sub"), lin, ""},
		{"windows call workdir", bash(`Remove-Item -Recurse x`, `D:\other`), win, "fs.delete_outside"},
		{"windows call workdir inside", bash(`Remove-Item -Recurse x`, `C:\Users\U\Work\sub`), win, ""},
		{"cwd key", call(harness.ToolRequest{Name: "run_command", Input: map[string]any{"cmd": "rm -r x", "cwd": "/tmp"}}), lin, "fs.delete_outside"},
		{"write path in system workdir", call(harness.ToolRequest{Name: "write_file", Input: map[string]any{"path": "hosts", "workdir": "/etc"}}), lin, "sys.dir"},
	}
	for _, c := range cases {
		h := Classify(c.c, c.x, Rules{})
		got := ""
		if h != nil {
			got = h.Rule
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q (%v; %+v)", c.name, got, c.want, h, c.c)
		}
	}
}
