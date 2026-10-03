// Package policy makes every tool permission decision. Adapters only
// translate native requests into harness.ToolRequest; the rules live here.
//
// The model is trust, not permission levels: Sin and all Plexus partners
// trust each other fully. Exactly one boundary exists, on by default (the
// "stranger guard"): a turn started by a Slack user who is neither Sin nor
// a partner gets conversation only. It may not use any tool (no reads, no
// commands, no writes, no network) except posting to its own thread. Stop is a control feature, not
// authorization: a stopped task tree gets no further tool calls.
package policy

import (
	"strings"

	"github.com/RCX1t7/plexus/internal/harness"
)

// Source says who started a turn.
type Source string

const (
	FromSin      Source = "sin"      // a configured owner (Sin)
	FromPartner  Source = "partner"  // another Plexus partner, on a trusted request
	FromStranger Source = "stranger" // anyone else, or a partner relaying a stranger's request
)

// Authority is who a turn acts for, and the task tree it belongs to.
type Authority struct {
	Source Source
	Root   string // task tree id ("" = none); revoked by stop
}

// Revoker reports whether a task tree was stopped.
type Revoker interface {
	AnyRevoked(ids ...string) (bool, error)
}

// Policy holds the machine-specific inputs to decisions.
type Policy struct {
	GOOS          string
	Home          string
	Workdir       string
	StrangerGuard bool
	Revoker       Revoker
}

// Restricted reports whether a turn from this source is under the guard.
func (p Policy) Restricted(a Authority) bool { return p.StrangerGuard && a.Source == FromStranger }

// Level is the native sandbox hint for a turn (defense in depth).
func (p Policy) Level(a Authority) harness.Level {
	if p.Restricted(a) {
		return harness.LevelChat // strangers: conversation, never write or execute
	}
	return harness.LevelFull
}

// Decide answers one tool request. A stop is re-checked first, so a
// stopped tree cannot run a new tool even if an interrupt was missed.
func (p Policy) Decide(a Authority, req harness.ToolRequest) harness.Decision {
	deny := func(r string) harness.Decision { return harness.Decision{Allow: false, Reason: r} }
	allow := func(r string) harness.Decision { return harness.Decision{Allow: true, Reason: r} }
	if a.Root != "" {
		if p.Revoker == nil {
			return deny("stop list unavailable")
		}
		if stopped, err := p.Revoker.AnyRevoked(a.Root); err != nil || stopped {
			return deny("this task was stopped")
		}
	}
	if p.Restricted(a) {
		if IsPostTool(req.Name) {
			return allow("posting to this thread")
		}
		return deny("a request from outside the team gets conversation only: no tools (reads, commands, writes or network)")
	}
	return allow("trusted")
}

// IsPostTool reports the plexus_post host tool under any harness's name for
// it (e.g. Claude's mcp__plexus__plexus_post): the one tool strangers keep.
func IsPostTool(name string) bool {
	return name == "plexus_post" || strings.HasSuffix(name, "__plexus_post")
}
