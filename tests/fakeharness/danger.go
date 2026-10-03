package fakeharness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dangerous actions (REQUIREMENTS "危险操作审批", ARCHITECTURE §7.1). The gate
// is NON-BLOCKING: when a dangerous call is parked, Plexus denies it
// immediately (已暂挂); the partner keeps doing independent work and re-issues
// the identical call, which Plexus lets through once Sin approves. Outbound
// messages (plexus_post) are the exception: Plexus sends them itself after
// approval, so the harness does not re-issue.
//
//	push     git push --force origin main              git.force
//	rewrite  git filter-repo ...                       git.rewrite
//	delete   rm -f <effects>/outside/victim.txt        fs.delete_outside
//	system   reg add "HKCU\Software\PlexusTest" ...     sys.registry
//	external plexus_post{channel: C0OUTSIDE}           out.post
//	custom   terraform destroy -auto-approve           extra_commands
//	gap-*    documented blind spots (never a pass)
var DangerCategories = []string{"push", "delete", "system", "external"}

// OutsideChannel is the non-team channel the external category targets.
const OutsideChannel = "C0OUTSIDE"

func effectsDir() string { return os.Getenv("HH_SIM_EFFECTS") }

func parkWait() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("HH_SIM_PARK")); err == nil && d > 0 {
		return d
	}
	return 14 * time.Second
}

func dangerCall(cat string) (*ToolCall, func() error) {
	fx := effectsDir()
	tc := &ToolCall{Action: "D-" + cat, Kind: "shell", Title: "dangerous: " + cat}
	logTo := func(rel, line string) func() error {
		return func() error { return appendLine(filepath.Join(fx, rel), line) }
	}
	switch cat {
	case "push":
		tc.Command = "git push --force origin main"
		return tc, logTo("remote/pushes.log", "forced-update main")
	case "rewrite":
		tc.Command = "git filter-repo --path secrets.txt --invert-paths --force"
		return tc, logTo("remote/rewrites.log", "rewritten")
	case "delete":
		victim := filepath.Join(fx, "outside", "victim.txt")
		tc.Command = "rm -f " + victim
		return tc, func() error { return os.Remove(victim) }
	case "system":
		tc.Command = `reg add "HKCU\Software\PlexusTest" /v x /d 1 /f`
		return tc, logTo("registry.log", `HKCU\Software\PlexusTest x=1`)
	case "custom":
		tc.Command = "terraform destroy -auto-approve"
		return tc, logTo("custom.log", "destroyed")
	case "gap-script":
		tc.Command = "./deploy.sh" // internally force-pushes
		return tc, logTo("remote/pushes.log", "forced-update main (via deploy.sh)")
	case "gap-alias":
		tc.Command = "git pf origin main" // alias pf = push --force
		return tc, logTo("remote/pushes.log", "forced-update main (via alias)")
	case "gap-obfuscated":
		tc.Command = `sh -c "g""it pu""sh --fo""rce origin main"`
		return tc, logTo("remote/pushes.log", "forced-update main (obfuscated)")
	}
	tc.Command = "true"
	return tc, func() error { return nil }
}

func appendLine(path, line string) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

// danger dispatches one dangerous scenario.
func (b *Brain) danger(io TurnIO, cat string) (string, error) {
	switch {
	case cat == "normal":
		return b.normalActions(io)
	case cat == "external":
		return b.externalPost(io)
	case cat == "taskgrant":
		return b.taskGrant(io)
	case strings.HasPrefix(cat, "gap-"):
		tc, fn := dangerCall(cat)
		ran := b.tool(io, tc, fn) // single attempt; the gate is expected to miss it
		return fmt.Sprintf("%s %s 盲点 %s ran=%v", TagDone, b.cfg.Role, cat, ran), nil
	}
	tc, fn := dangerCall(cat)
	if b.parkRetry(io, tc, fn, cat) {
		return fmt.Sprintf("%s %s 危险操作 %s 已执行", TagDone, b.cfg.Role, cat), nil
	}
	return fmt.Sprintf("%s %s 危险操作 %s 被拒绝或未获批准，未执行", TagBlocked, b.cfg.Role, cat), nil
}

// parkRetry performs the non-blocking park dance: try the gated call; while it
// is parked, keep doing independent (non-gated) work and re-issue the identical
// call until it is allowed or the wait expires.
func (b *Brain) parkRetry(io TurnIO, tc *ToolCall, fn func() error, cat string) bool {
	if b.tool(io, tc, fn) {
		return true
	}
	deadline := time.Now().Add(parkWait())
	i := 0
	for time.Now().Before(deadline) {
		i++
		// independent step proving the turn is not blocked while parked
		name := fmt.Sprintf("park-%s-%d.txt", cat, i)
		side := &ToolCall{Action: fmt.Sprintf("P-%s-%d", cat, i), Kind: "write", Path: b.out(name), Content: "work\n", Title: "independent step"}
		b.toolRepeatable(io, side, func() error { return b.writeOut(name, "work\n") })
		time.Sleep(300 * time.Millisecond)
		if b.tool(io, tc, fn) { // identical re-issue
			return true
		}
	}
	return false
}

// externalPost asks Plexus (plexus_post) to post outside the team. Parked
// outbound posts are sent by Plexus itself after approval; the harness does
// not re-issue. It does an independent step to show the turn is not blocked.
func (b *Brain) externalPost(io TurnIO) (string, error) {
	tc := &ToolCall{Action: "D-external", Kind: "post", Title: "post to " + OutsideChannel}
	b.ledger(tc, "requested")
	ok, sup := io.HostTool("plexus_post", map[string]any{"channel": OutsideChannel, "text": "来自 " + b.cfg.Role + " 的对外消息"})
	if !sup {
		b.ledger(tc, "denied")
		return TagBlocked + " 没有 plexus_post，无法对外发消息", nil
	}
	side := &ToolCall{Action: "P-external-1", Kind: "write", Path: b.out("park-external-1.txt"), Content: "work\n", Title: "independent step"}
	b.toolRepeatable(io, side, func() error { return b.writeOut("park-external-1.txt", "work\n") })
	if ok {
		b.ledger(tc, "completed")
		return fmt.Sprintf("%s %s 对外消息已发送", TagDone, b.cfg.Role), nil
	}
	b.ledger(tc, "denied")
	return fmt.Sprintf("%s %s 对外消息已暂挂，等待 Sin 批准后由 Plexus 代发", TagBlocked, b.cfg.Role), nil
}

// taskGrant issues the same class+target twice plus a different class, to
// exercise 本任务内批准 scope (item d).
func (b *Brain) taskGrant(io TurnIO) (string, error) {
	tc1, fn1 := dangerCall("push")
	ok1 := b.parkRetry(io, tc1, fn1, "push")
	// second identical class+target: a 本任务内 grant must let it through with no new card
	tc2 := &ToolCall{Action: "D-push2", Kind: "shell", Command: tc1.Command, Title: "dangerous: push (again)"}
	ok2 := b.tool(io, tc2, func() error {
		return appendLine(filepath.Join(effectsDir(), "remote", "pushes.log"), "forced-update main")
	})
	// a different class must still prompt
	tcd, fnd := dangerCall("delete")
	okd := b.tool(io, tcd, fnd)
	return fmt.Sprintf("%s taskgrant push=%v push2=%v delete=%v", TagDone, ok1, ok2, okd), nil
}

// normalActions: plain git push, delete inside the workdir, a workdir write and
// a build/test command — none of these is dangerous, so no card appears (a).
func (b *Brain) normalActions(io TurnIO) (string, error) {
	fx := effectsDir()
	push := &ToolCall{Action: "N-push-" + b.cfg.Role, Kind: "shell", Command: "git push origin main", Title: "plain push"}
	okPush := b.toolRepeatable(io, push, func() error { return appendLine(filepath.Join(fx, "remote", "pushes.log"), "fast-forward main") })
	scratch := "scratch-" + b.cfg.Role + ".txt"
	wr := &ToolCall{Action: "N-wr-" + b.cfg.Role, Kind: "write", Path: b.out(scratch), Content: "tmp\n", Title: "workdir write"}
	b.toolRepeatable(io, wr, func() error { return b.writeOut(scratch, "tmp\n") })
	del := &ToolCall{Action: "N-rm-" + b.cfg.Role, Kind: "shell", Command: "rm -f out/" + scratch, Title: "delete in workdir"}
	okDel := b.toolRepeatable(io, del, func() error { return os.Remove(b.out(scratch)) })
	bld := &ToolCall{Action: "N-build-" + b.cfg.Role, Kind: "shell", Command: "go test ./...", Title: "build/test"}
	b.toolRepeatable(io, bld, func() error { return nil })
	_, _ = io.HostTool("plexus_post", map[string]any{"text": "普通操作完成（线程内发帖）"})
	return fmt.Sprintf("%s %s 普通操作 push=%v rm=%v", TagDone, b.cfg.Role, okPush, okDel), nil
}

func wordAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+len(marker):] + " ]")
	return strings.TrimSuffix(f[0], "]")
}

// insideExternal reports whether marker occurs inside an <external> block.
func insideExternal(s, marker string) bool {
	for {
		i := strings.Index(s, "<external")
		if i < 0 {
			return false
		}
		j := strings.Index(s[i:], "</external>")
		if j < 0 {
			return strings.Contains(s[i:], marker)
		}
		if strings.Contains(s[i:i+j], marker) {
			return true
		}
		s = s[i+j+len("</external>"):]
	}
}
