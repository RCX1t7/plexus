// Package handoff is the six-field record that travels with a task when one
// partner hands work to another. It is a plain record, not authorization:
// it says what the work is and when it is done. It is stored in the hub's
// database and shown in Slack as a compact card.
//
// Partners with host tools use plexus_delegate; others mention a peer and
// write a block like:
//
//	HANDOFF
//	task: Add retries to the uploader
//	inputs: uploader.go; issue #12 https://example/12
//	done_when: go test ./uploader passes with a flaky-server test
//	evidence: "timeout after 30s" in run 4411
//	tried_failed: raising the timeout (still flaky)
//	owner_if_stuck: <@U0123ABCD>
//
// List fields take ";"-separated items or repeated keys.
package handoff

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Record is the six-field handoff.
type Record struct {
	Task         string   `json:"task"`                   // one line
	Inputs       []string `json:"inputs,omitempty"`       // named or linked
	DoneWhen     string   `json:"done_when"`              // written before work starts
	Evidence     []string `json:"evidence,omitempty"`     // quoted or linked
	TriedFailed  []string `json:"tried_failed,omitempty"` // what did not work
	OwnerIfStuck string   `json:"owner_if_stuck"`         // who decides when blocked
}

// Hint is appended to every bot's persona so harnesses know the format.
const Hint = "When you hand work to another bot in Slack, mention it and include a block that starts with a line HANDOFF followed by the lines task:, inputs:, done_when:, evidence:, tried_failed:, owner_if_stuck: (write done_when before the work starts). " +
	"With Plexus tools: plexus_delegate hands off a node; the assignee calls plexus_ack (done_when is locked from then on) and plexus_deliver with the node; " +
	"the delegator then checks the delivery against done_when and calls plexus_review with accept or reopen and a reason. A delegated node is yours until you accept it."

// Parse extracts a HANDOFF block from text. It returns the record, the text
// without the block, and whether a block was found.
func Parse(text string) (Record, string, bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		t := strings.Trim(strings.TrimSpace(l), "*`:")
		if strings.EqualFold(t, "handoff") {
			start = i
			break
		}
	}
	if start < 0 {
		return Record{}, text, false
	}
	var r Record
	end := start + 1
	for ; end < len(lines); end++ {
		k, v, ok := strings.Cut(strings.TrimSpace(lines[end]), ":")
		if !ok {
			break
		}
		k = strings.ToLower(strings.Trim(strings.TrimSpace(strings.TrimLeft(k, "-• ")), "*_`"))
		v = strings.TrimSpace(v)
		switch k {
		case "task":
			r.Task = oneLine(v)
		case "inputs":
			r.Inputs = append(r.Inputs, split(v)...)
		case "done_when", "done when":
			r.DoneWhen = v
		case "evidence":
			r.Evidence = append(r.Evidence, split(v)...)
		case "tried_failed", "tried/failed", "tried":
			r.TriedFailed = append(r.TriedFailed, split(v)...)
		case "owner_if_stuck", "owner if stuck":
			r.OwnerIfStuck = v
		default:
			goto done
		}
	}
done:
	rest := strings.TrimSpace(strings.Join(append(append([]string{}, lines[:start]...), lines[end:]...), "\n"))
	return r, rest, true
}

func split(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ";") {
		if p = strings.TrimSpace(p); p != "" && p != "-" && !strings.EqualFold(p, "none") {
			out = append(out, p)
		}
	}
	return out
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// Fill sets defaults: the task from the message's first line and the
// delegator (the partner handing the work off) as owner_if_stuck.
func (r *Record) Fill(message, delegator string) {
	if r.Task == "" {
		r.Task = oneLine(message)
	}
	if r.OwnerIfStuck == "" && delegator != "" {
		r.OwnerIfStuck = "<@" + delegator + ">"
	}
}

// Missing lists required fields that are empty.
func (r Record) Missing() []string {
	var m []string
	if r.Task == "" {
		m = append(m, "task")
	}
	if r.DoneWhen == "" {
		m = append(m, "done_when")
	}
	if r.OwnerIfStuck == "" {
		m = append(m, "owner_if_stuck")
	}
	return m
}

// Card renders the compact Slack card. taskID is the delegated task id.
func (r Record) Card(taskID, to string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📋 *Handoff*")
	if to != "" {
		fmt.Fprintf(&b, " → <@%s>", to)
	}
	if taskID != "" {
		fmt.Fprintf(&b, " · task `%s`", taskID)
	}
	fmt.Fprintf(&b, "\n> *Task:* %s", orMissing(r.Task))
	if len(r.Inputs) > 0 {
		fmt.Fprintf(&b, "\n> *Inputs:* %s", strings.Join(r.Inputs, " · "))
	}
	fmt.Fprintf(&b, "\n> *Done when:* %s", orMissing(r.DoneWhen))
	if len(r.Evidence) > 0 {
		fmt.Fprintf(&b, "\n> *Evidence:* %s", strings.Join(r.Evidence, " · "))
	}
	if len(r.TriedFailed) > 0 {
		fmt.Fprintf(&b, "\n> *Tried/failed:* %s", strings.Join(r.TriedFailed, " · "))
	}
	fmt.Fprintf(&b, "\n> *If stuck:* %s", orMissing(r.OwnerIfStuck))
	return b.String()
}

func orMissing(s string) string {
	if s == "" {
		return "_missing — state it before starting_"
	}
	return s
}

// Prompt renders the record for the receiving harness. node is the
// delegated node id ("" for a text-only handoff).
func (r Record) Prompt(node string) string {
	b, _ := json.Marshal(r)
	s := "[Handoff record for this task: " + string(b) + ". If done_when is empty, write it in your first reply before doing the work."
	if node != "" {
		s += " Node `" + node + "`: call plexus_ack with this node before you start (done_when is locked from then on), and plexus_deliver with this node when done_when is met."
	}
	return s + "]"
}
