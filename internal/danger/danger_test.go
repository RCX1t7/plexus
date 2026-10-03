package danger

import (
	"encoding/base64"
	"testing"
	"unicode/utf16"

	"github.com/RCX1t7/plexus/internal/harness"
)

func enc(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestClassify(t *testing.T) {
	win := Ctx{GOOS: "windows", Workdir: `C:\Users\sin\work`}
	pushed := win
	pushed.HeadPushed = func() bool { return true }
	sh := func(cmd string) Call { return Call{Kind: harness.ToolShell, Command: cmd} }
	cases := []struct {
		name string
		c    Call
		x    Ctx
		want string // "" = no hit
	}{
		{"push", sh("git push origin main"), win, ""},
		{"push -u", sh("git push -u origin feature"), win, ""},
		{"force", sh("git push --force origin main"), win, "git.force"},
		{"-f", sh("git push -f"), win, "git.force"},
		{"lease", sh("git push --force-with-lease=main origin main"), win, "git.force"},
		{"plus refspec", sh("git push origin +main"), win, "git.force"},
		{"delete ref", sh("git push origin :old"), win, "git.force"},
		{"mirror", sh("git -C repo push --mirror"), win, "git.force"},
		{"chained", sh("git add . && git commit -m x && git push -f"), win, "git.force"},
		{"cmd /c", sh(`cmd /c "git push --force"`), win, "git.force"},
		{"sudo", sh("sudo git push -f"), win, "git.force"},
		{"git -C", sh(`git -C C:\other\repo push --force`), win, "git.force"},
		{"git -c k=v", sh("git -c http.extraHeader=x push -f origin main"), win, "git.force"},
		{"global flags", sh("git --no-pager --git-dir=.git -C . push origin +main"), win, "git.force"},
		{"lease after -c", sh("git -c core.askpass=true push --force-with-lease origin main"), win, "git.force"},
		{"full +refspec", sh("git push origin +refs/heads/a:refs/heads/b"), win, "git.force"},
		{"push option value", sh("git push -o ci.skip origin +main"), win, "git.force"},
		{"combined short", sh("git push -uf origin main"), win, "git.force"},
		{"delete refspec", sh("git push origin :old"), win, "git.force"},
		{"opaque exec", Call{Kind: harness.ToolShell, Name: "execute"}, win, "exec.opaque"},
		{"plain push with -c", sh("git -c color.ui=never push -u origin main"), win, ""},
		{"env prefix", sh("env GIT_TRACE=1 git push --force"), win, "git.force"},
		{"bash -c", sh(`bash -lc 'git push --force'`), win, "git.force"},
		{"encoded", sh("powershell -NoProfile -EncodedCommand " + enc("git push --force")), win, "git.force"},
		{"filter-repo", sh("git filter-repo --path secret --invert-paths"), win, "git.rewrite"},
		{"rebase unpushed", sh("git rebase -i HEAD~3"), win, ""},
		{"rebase pushed", sh("git rebase -i HEAD~3"), pushed, "git.rewrite"},
		{"rebase continue", sh("git rebase --continue"), pushed, ""},
		{"amend pushed", sh("git commit --amend --no-edit"), pushed, "git.rewrite"},
		{"rm inside", sh(`rm -rf build\out`), win, ""},
		{"rm abs inside", sh(`del /s /q C:\Users\sin\work\tmp`), win, ""},
		{"rm outside", sh(`rm -rf C:\Users\sin\other`), win, "fs.delete_outside"},
		{"rm parent", sh(`Remove-Item -Recurse ..\x`), win, "fs.delete_outside"},
		{"rm var", sh(`rm -rf $HOME/x`), win, "fs.delete_outside"},
		{"rm envvar", sh(`rd /s /q %USERPROFILE%\x`), win, "fs.delete_outside"},
		{"reg query", sh(`reg query HKCU\Software\X`), win, ""},
		{"reg add", sh(`reg add HKCU\Software\X /v a /d b`), win, "sys.registry"},
		{"ps registry", sh(`Set-ItemProperty -Path HKLM:\SOFTWARE\X -Name a -Value 1`), win, "sys.registry"},
		{"service", sh(`sc.exe create foo binPath= C:\foo.exe`), win, "sys.service"},
		{"sc query", sh(`sc query foo`), win, ""},
		{"task", sh(`schtasks /Create /TN x /TR y`), win, "sys.task"},
		{"setx user", sh(`setx FOO bar`), win, ""},
		{"setx machine", sh(`setx FOO bar /M`), win, "sys.env"},
		{"env machine", sh(`[Environment]::SetEnvironmentVariable('A','b','Machine')`), win, "sys.env"},
		{"msi", sh(`msiexec /i x.msi`), win, "sys.installer"},
		{"winget", sh(`winget install Git.Git`), win, "sys.installer"},
		{"webhook", sh(`curl -X POST https://hooks.slack.com/services/T/B/X -d '{}'`), win, "out.webhook"},
		{"plain curl", sh(`curl https://example.com`), win, ""},
		{"mail", sh(`Send-MailMessage -To a@b.c -Subject x`), win, "out.mail"},
		{"write sysdir", Call{Kind: harness.ToolWrite, Paths: []string{`C:\Windows\System32\drivers\etc\hosts`}}, win, "sys.dir"},
		{"write workdir", Call{Kind: harness.ToolWrite, Paths: []string{`C:\Users\sin\work\a.go`}}, win, ""},
		{"mcp send", Call{Kind: harness.ToolOther, Name: "mcp__slack__slack_send_message"}, win, "out.mcp"},
		{"mcp read", Call{Kind: harness.ToolOther, Name: "mcp__slack__slack_read_channel"}, win, ""},
		{"env prefix", sh(`GIT_TRACE=1 git push --force`), win, "git.force"},
		{"linux rm outside", sh(`rm -rf /home/sin/other`), Ctx{GOOS: "linux", Workdir: "/home/sin/work"}, "fs.delete_outside"},
		{"linux rm inside", sh(`rm -rf ./dist node_modules`), Ctx{GOOS: "linux", Workdir: "/home/sin/work"}, ""},
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

func TestRulesConfig(t *testing.T) {
	x := Ctx{GOOS: "windows", Workdir: `C:\w`}
	force := Call{Kind: harness.ToolShell, Command: "git push --force"}
	if Classify(force, x, Rules{Disable: []string{"git.force"}}) != nil {
		t.Fatal("disabled rule hit")
	}
	if Classify(Call{Kind: harness.ToolShell, Command: "reg add X"}, x, Rules{Disable: []string{"sys."}}) != nil {
		t.Fatal("prefix disable")
	}
	off := false
	if Classify(force, x, Rules{UseDefaults: &off}) != nil {
		t.Fatal("defaults off")
	}
	if h := Classify(Call{Kind: harness.ToolShell, Command: "terraform apply -auto-approve"}, x,
		Rules{ExtraCommands: []string{`^terraform apply`}}); h == nil || h.Rule != "extra.command" {
		t.Fatal("extra command", h)
	}
	if Fingerprint(force) != Fingerprint(Call{Kind: harness.ToolShell, Command: "git  push   --force"}) {
		t.Fatal("fingerprint not whitespace-stable")
	}
}
