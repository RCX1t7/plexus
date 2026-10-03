package slackbot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RCX1t7/plexus/internal/handoff"
	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/policy"
)

// hostTool runs one Plexus host tool call for thread t.
func (w *Worker) hostTool(t *thread, j job, ev harness.Event) harness.HostResult {
	fail := func(f string, a ...any) harness.HostResult {
		return harness.HostResult{Text: fmt.Sprintf(f, a...), IsError: true}
	}
	if t.isStopped() {
		return fail("this task was stopped")
	}
	callID := ev.Name
	if ev.Tool != nil && ev.Tool.CallID != "" {
		callID = ev.Tool.CallID
	}
	switch strings.TrimPrefix(ev.Name, "mcp__plexus__") {
	case "plexus_post":
		var a struct {
			Text string `json:"text"`
		}
		if err := toolArgs(ev, &a); err != nil || strings.TrimSpace(a.Text) == "" {
			return fail("text is required")
		}
		w.post(t, j, RequestID(w.Bot.Name, t.key, "post", j.in.TS, callID), "post", a.Text)
		return harness.HostResult{Text: "posted"}
	case "plexus_delegate":
		var a struct {
			To string `json:"to"`
			handoff.Record
		}
		if err := toolArgs(ev, &a); err != nil {
			return fail("bad arguments: %v", err)
		}
		to := w.Peers.ID(strings.Trim(a.To, "<@>"))
		if to == "" || to == w.SelfID {
			return fail("unknown partner %q", a.To)
		}
		rec := a.Record
		rec.Fill("", firstOwner(w.Owners))
		if m := rec.Missing(); len(m) > 0 {
			return fail("missing %s", strings.Join(m, ", "))
		}
		id := RequestID(w.Bot.Name, t.key, "handoff", j.in.TS, callID)
		w.saveHandoff(t, j, id, to, rec)
		t.mu.Lock()
		t.pendingHandoff = id
		t.mu.Unlock()
		w.post(t, j, id, "handoff", fmt.Sprintf("<@%s> %s\n%s", to, rec.Task, rec.Card(id, to)))
		return harness.HostResult{Text: "handed off as " + id}
	case "plexus_deliver":
		var a struct {
			Summary   string   `json:"summary"`
			Artifacts []string `json:"artifacts"`
			Evidence  []string `json:"evidence"`
		}
		if err := toolArgs(ev, &a); err != nil || a.Summary == "" {
			return fail("summary is required")
		}
		card, err := w.deliveryCard(a.Summary, a.Artifacts, a.Evidence)
		if err != nil {
			return fail("%v", err)
		}
		w.post(t, j, RequestID(w.Bot.Name, t.key, "deliver", j.in.TS, callID), "deliver", card)
		return harness.HostResult{Text: "delivered"}
	case "plexus_stop_tree":
		if j.auth.Source != policy.FromSin || j.autonomous {
			return fail("only Sin can stop a task tree")
		}
		go w.stopTree(context.Background(), j.auth.Root, j.in)
		return harness.HostResult{Text: "stopping"}
	}
	return fail("unknown tool %s", ev.Name)
}

// deliveryCard renders the uniform delivery post and writes
// out/MANIFEST.sha256 for artifacts under out/.
func (w *Worker) deliveryCard(summary string, artifacts, evidence []string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "📦 *Delivered* — %s", summary)
	var manifest []string
	root, _ := filepath.Abs(w.Policy.Workdir)
	for _, a := range artifacts {
		p := a
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("artifact %s is outside the workdir", a)
		}
		sum, err := fileSHA256(p)
		if err != nil {
			return "", fmt.Errorf("artifact %s: %v", a, err)
		}
		rel = filepath.ToSlash(rel)
		fmt.Fprintf(&b, "\n> `%s` sha256 `%s`", rel, sum)
		if strings.HasPrefix(rel, "out/") {
			manifest = append(manifest, sum+"  "+strings.TrimPrefix(rel, "out/"))
		}
	}
	for _, e := range evidence {
		fmt.Fprintf(&b, "\n> evidence: %s", e)
	}
	if len(manifest) > 0 {
		sort.Strings(manifest)
		_ = os.WriteFile(filepath.Join(root, "out", "MANIFEST.sha256"), []byte(strings.Join(manifest, "\n")+"\n"), 0o644)
	}
	return b.String(), nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
