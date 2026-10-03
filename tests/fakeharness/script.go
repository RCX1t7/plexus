package fakeharness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Handoff is the six-field record every cross-bot handoff carries
// (ARCHITECTURE §7.1 / team rule): task, inputs, done_when, evidence,
// tried_failed, owner_if_stuck. It is passed as plexus_delegate arguments
// (§6.2 signature: to, task, inputs[], done_when[], evidence[], owner_if_stuck,
// tried_failed[]). With signed tokens dropped by Sin, the acceptance suite
// expects Plexus to put the record into the HANDOFF post's Slack metadata
// (event_payload.handoff, PROPOSAL - flagged in ACCEPTANCE.md).
type Handoff struct {
	To           string   `json:"to"` // Slack user id of the receiving bot
	Task         string   `json:"task"`
	Inputs       []string `json:"inputs"`
	DoneWhen     []string `json:"done_when"`
	Evidence     []string `json:"evidence"`
	TriedFailed  []string `json:"tried_failed"`
	OwnerIfStuck string   `json:"owner_if_stuck"`
	Review       bool     `json:"review"` // the receiver reviews/verifies the inputs (reviewer != author)
}

// HandoffFields are the six mandatory fields, by their JSON names.
var HandoffFields = []string{"task", "inputs", "done_when", "evidence", "tried_failed", "owner_if_stuck"}

func (h Handoff) args(drop string) map[string]any {
	m := map[string]any{"to": h.To, "task": h.Task, "inputs": h.Inputs, "done_when": h.DoneWhen,
		"evidence": h.Evidence, "tried_failed": h.TriedFailed, "owner_if_stuck": h.OwnerIfStuck, "review": h.Review}
	if drop != "" {
		delete(m, drop)
	}
	return m
}

// Text renders the record for humans (also the no-host-tools fallback).
func (h Handoff) Text(drop string) string {
	line := func(k string, v ...string) string {
		if k == drop {
			return ""
		}
		return fmt.Sprintf("\n• %s: %s", k, strings.Join(v, "；"))
	}
	return line("task", h.Task) + line("inputs", h.Inputs...) + line("done_when", h.DoneWhen...) +
		line("evidence", h.Evidence...) + line("tried_failed", h.TriedFailed...) + line("owner_if_stuck", h.OwnerIfStuck)
}

// say publishes text for a segment. Turns started by a human post their
// final text; autonomous turns (triggered by a partner's post) publish only
// through plexus_post (ARCHITECTURE §6.2). Without mounted Plexus tools the
// text falls back to the final answer.
func (b *Brain) say(io TurnIO, sg segment, text string) string {
	if isHuman(sg) {
		return text
	}
	if _, sup := io.HostTool("plexus_post", map[string]any{"text": text}); sup {
		return ""
	}
	return text
}

func (b *Brain) hostOr(io TurnIO, name string, args map[string]any, fallback string) string {
	if _, sup := io.HostTool(name, args); sup {
		return ""
	}
	return fallback
}

func (b *Brain) delegate(io TurnIO, h Handoff, text string) string {
	args := h.args(b.cfg.DropField)
	args["text"] = text
	return b.hostOr(io, "plexus_delegate", args, text+h.Text(b.cfg.DropField))
}

func (b *Brain) ack(io TurnIO, node string, doneWhen []string) string {
	return b.hostOr(io, "plexus_ack", map[string]any{"node": node, "done_when": doneWhen,
		"evidence": []string{"按 done_when 逐条给出命令与结果"}}, "ACK "+node+" done_when: "+strings.Join(doneWhen, "；"))
}

func (b *Brain) review(io TurnIO, node, reason string) string {
	return b.hostOr(io, "plexus_review", map[string]any{"node": node, "decision": "accept", "reason": reason, "missing": []string{}},
		"ACCEPT "+node+"："+reason)
}

func (b *Brain) deliver(io TurnIO, node, summary string, files, evidence []string) string {
	var arts []any
	var lines []string
	for _, f := range files {
		sum, _ := fileSHA(filepath.Join(b.cfg.Workspace, f))
		arts = append(arts, map[string]any{"path": f, "sha256": sum})
		lines = append(lines, fmt.Sprintf("%s  %s", sum, f))
	}
	args := map[string]any{"node": node, "summary": summary, "artifacts": arts, "evidence": evidence}
	if b.cfg.EditDoneWhen && node != "root" {
		args["done_when"] = []string{"（ACK 之后改写的验收条件）只要文件存在即可"}
	}
	return b.hostOr(io, "plexus_deliver", args,
		fmt.Sprintf("%s\n```\n%s\n```\n证据：%s", summary, strings.Join(lines, "\n"), strings.Join(evidence, "；")))
}

// RootDoneWhen is what the root owner (claude) writes at ACK, before any tool.
var RootDoneWhen = []string{"out/result.json 字段与 Owner 确认的一致", "codex 独立校验 PASS（out/test-report.txt）",
	"dsh 审查通过（out/REVIEW.md）", "out/MANIFEST.sha256 覆盖全部产物且 sha256sum -c 通过"}

// ------------------------------------------------------------------ roles

func (b *Brain) claude(io TurnIO, sg segment) (string, error) {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	switch {
	case sg.From == "owner" && strings.Contains(sg.Body, TagGoal):
		if !b.done("root-ack") { // done_when is written before any work
			add(b.ack(io, "root", RootDoneWhen))
			b.mark("root-ack")
		}
		add(b.say(io, sg, TagSplit+" codex 负责独立测试校验（out/test-report.txt），dsh 负责审查（out/REVIEW.md），我（claude）负责实现 out/result.json 并汇总交付。请确认。"))
	case sg.From == "codex" && strings.Contains(sg.Body, TagAgree):
		if b.answer() == "" {
			ans, ok := io.Ask(Asked{ID: "q1", Header: QuestionHeader, Text: QuestionText, Options: QuestionOptions})
			if !ok {
				return b.say(io, sg, TagBlocked+" 未得到字段名确认"), nil
			}
			b.mu.Lock()
			b.s.Answer = ans
			b.persist()
			b.mu.Unlock()
		}
		tc := &ToolCall{Action: "A1", Kind: "write", Path: b.out("result.json"), Title: "write out/result.json"}
		if !b.tool(io, tc, b.writeResult) {
			return b.say(io, sg, TagBlocked+" A1 未执行"), nil
		}
		sum, _ := fileSHA(b.out("result.json"))
		h := Handoff{To: b.cfg.Peers["codex"], Task: TagVerifyReq + " 独立校验 out/result.json",
			Inputs: []string{"input.txt", "out/result.json"}, DoneWhen: []string{"out/test-report.txt 以 PASS 开头", "lines/words/bytes 与独立子进程统计一致"},
			Evidence: []string{"A1 写入 out/result.json sha256=" + sum}, TriedFailed: []string{"无（首次实现）"}, OwnerIfStuck: b.at("claude"), Review: true}
		add(b.delegate(io, h, fmt.Sprintf("%s %s 已生成 out/result.json，请独立校验。", TagVerifyReq, b.at("codex"))))
	case sg.From == "codex" && strings.Contains(sg.Body, TagVerified):
		if !b.done("review-H1") {
			add(b.review(io, "H1", "test-report.txt 为 PASS，与 done_when 一致"))
			b.mark("review-H1")
		}
		h := Handoff{To: b.cfg.Peers["dsh"], Task: TagReviewReq + " 审查 out/result.json 与 out/test-report.txt",
			Inputs: []string{"out/result.json", "out/test-report.txt"}, DoneWhen: []string{"out/REVIEW.md 写明两个文件的 sha256", "结论为通过或列出问题"},
			Evidence: []string{"codex 校验 PASS（out/test-report.txt）"}, TriedFailed: []string{"无"}, OwnerIfStuck: b.at("claude"), Review: true}
		add(b.delegate(io, h, fmt.Sprintf("%s %s 请审查实现与测试结果。", TagReviewReq, b.at("dsh"))))
	case sg.From == "dsh" && strings.Contains(sg.Body, TagReviewed):
		if !b.done("review-H2") {
			add(b.review(io, "H2", "REVIEW.md 覆盖两个文件且结论通过"))
			b.mark("review-H2")
		}
		tc := &ToolCall{Action: "A4", Kind: "write", Path: b.out("MANIFEST.sha256"), Title: "write out/MANIFEST.sha256"}
		if !b.tool(io, tc, b.writeManifest) {
			return b.say(io, sg, TagBlocked+" A4 未执行"), nil
		}
		files := []string{"out/result.json", "out/test-report.txt", "out/REVIEW.md", "out/MANIFEST.sha256"}
		ev := []string{"codex: count input.txt → PASS（out/test-report.txt）", "dsh: 审查通过（out/REVIEW.md）", "sha256sum -c out/MANIFEST.sha256 → OK"}
		mani, _ := os.ReadFile(b.out("MANIFEST.sha256"))
		add(b.deliver(io, "root", fmt.Sprintf("%s %s 产物已就绪，各产物 SHA256：\n%sMANIFEST 见 out/MANIFEST.sha256", TagDeliver, b.at("owner"), string(mani)), files, ev))
	}
	return strings.Join(out, "\n"), nil
}

func (b *Brain) codex(io TurnIO, sg segment) (string, error) {
	switch {
	case sg.From == "owner" && strings.Contains(sg.Body, TagGoal):
		return b.say(io, sg, TagPlan+"我（codex）负责独立测试校验：用独立子进程重新统计 input.txt，并与 out/result.json 比对。"), nil
	case sg.From == "claude" && strings.Contains(sg.Body, TagSplit):
		return b.say(io, sg, TagAgree+" codex 负责测试校验。"), nil
	case sg.From == "claude" && strings.Contains(sg.Body, TagVerifyReq):
		var out []string
		if !b.done("ack-H1") {
			if s := b.ack(io, "H1", []string{"out/test-report.txt 以 PASS 开头", "lines/words/bytes 与独立子进程统计一致"}); s != "" {
				out = append(out, s)
			}
			b.mark("ack-H1")
		}
		tc := &ToolCall{Action: "A2", Kind: "shell", Command: b.cfg.Exe + " count input.txt", Title: "independent count"}
		var report string
		if !b.tool(io, tc, func() error { var err error; report, err = b.verify(); return err }) {
			return b.say(io, sg, TagBlocked+" A2 未执行"), nil
		}
		if report == "" { // done in an earlier, interrupted turn
			raw, _ := os.ReadFile(b.out("test-report.txt"))
			report = string(raw)
		}
		if !strings.HasPrefix(report, "PASS") {
			return b.say(io, sg, TagVerifyBad+" "+strings.TrimSpace(report)), nil
		}
		stats := strings.TrimSpace(strings.TrimPrefix(report, "PASS "))
		if s := b.deliver(io, "H1", fmt.Sprintf("%s %s out/result.json 与独立统计一致（%s）。", TagVerified, b.at("claude"), stats),
			[]string{"out/test-report.txt"}, []string{tc.Command + " → PASS " + stats}); s != "" {
			out = append(out, s)
		}
		return strings.Join(out, "\n"), nil
	}
	return "", nil
}

func (b *Brain) dsh(io TurnIO, sg segment) (string, error) {
	switch {
	case sg.From == "owner" && strings.Contains(sg.Body, TagGoal):
		return b.say(io, sg, TagPlan+"我（dsh）负责审查实现与测试结果，结论写入 out/REVIEW.md。"), nil
	case sg.From == "claude" && strings.Contains(sg.Body, TagSplit):
		return b.say(io, sg, TagAgree+" dsh 负责审查。"), nil
	case sg.From == "claude" && strings.Contains(sg.Body, TagReviewReq):
		var out []string
		if !b.done("ack-H2") {
			if s := b.ack(io, "H2", []string{"out/REVIEW.md 写明两个文件的 sha256", "结论为通过或列出问题"}); s != "" {
				out = append(out, s)
			}
			b.mark("ack-H2")
		}
		tc := &ToolCall{Action: "A3", Kind: "write", Path: b.out("REVIEW.md"), Title: "write out/REVIEW.md"}
		if !b.tool(io, tc, b.writeReview) {
			return b.say(io, sg, TagBlocked+" A3 未执行"), nil
		}
		if s := b.deliver(io, "H2", fmt.Sprintf("%s %s 已审查，结论见 out/REVIEW.md。", TagReviewed, b.at("claude")),
			[]string{"out/REVIEW.md"}, []string{"sha256(out/result.json), sha256(out/test-report.txt) 已写入 REVIEW.md"}); s != "" {
			out = append(out, s)
		}
		return strings.Join(out, "\n"), nil
	}
	return "", nil
}
