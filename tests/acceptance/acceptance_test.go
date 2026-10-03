package acceptance

import (
	"slices"
	"testing"

	"github.com/RCX1t7/plexus/tests/simkit"
)

// TestAcceptanceSim is the full suite against the REAL hub. It skips (never
// passes vacuously) until PLEXUS_BIN points at a build of cmd/plexus.
func TestAcceptanceSim(t *testing.T) {
	spec, skip := simkit.HubSpecFromEnv()
	if skip != "" {
		t.Skip(skip)
	}
	var r simkit.Report
	e := newEnvOpts(t, spec, simkit.Opts{Long: map[string]string{"claude": "tick", "codex": "tool", "dsh": "gen"}, Misbehave: []string{"codex"}})
	e.RunMain(&r)
	e.RunStop(&r)
	e.Teardown(&r)
	t.Logf("acceptance report (%s):\n%s", spec.Name, r.String())
	if f := r.Failed(); len(f) > 0 {
		t.Fatalf("failed checks: %v (hub log tail:\n%s)", f, e.Hub.LogTail(2000))
	}
}

// TestSuiteSelfCheck_Main validates the collaboration/restart/handoff checks
// against the reference STUB hub. It validates the suite, not Plexus.
func TestSuiteSelfCheck_Main(t *testing.T) {
	skipSelfCheck(t)
	e := newEnv(t, simkit.RefHubSpec(refhubBin))
	var r simkit.Report
	e.RunMain(&r)
	e.Teardown(&r)
	t.Logf("suite self-check (main) against refhub(stub):\n%s", r.String())
	if f := r.Failed(); len(f) > 0 {
		t.Fatalf("suite self-check failed: %v\nhub log:\n%s", f, e.Hub.LogTail(3000))
	}
}

// TestSuiteSelfCheck_Stop validates the stop sequence against the stub.
func TestSuiteSelfCheck_Stop(t *testing.T) {
	skipSelfCheck(t)
	e := newEnvOpts(t, simkit.RefHubSpec(refhubBin),
		simkit.Opts{Long: map[string]string{"claude": "tick", "codex": "tool", "dsh": "gen"}, Misbehave: []string{"codex"}})
	var r simkit.Report
	e.RunStop(&r)
	e.Teardown(&r)
	t.Logf("suite self-check (stop):\n%s", r.String())
	if f := r.Failed(); len(f) > 0 {
		t.Fatalf("stop self-check failed: %v\nhub log:\n%s", f, e.Hub.LogTail(3000))
	}
}

// TestSuiteSelfCheck_SubStop validates partner sub-tree stop + rogue-stop.
func TestSuiteSelfCheck_SubStop(t *testing.T) {
	skipSelfCheck(t)
	e := newEnvOpts(t, simkit.RefHubSpec(refhubBin), simkit.Opts{Long: map[string]string{"codex": "tool"}})
	var r simkit.Report
	e.RunSubStop(&r)
	e.Teardown(&r)
	t.Logf("suite self-check (substop):\n%s", r.String())
	if f := r.Failed(); len(f) > 0 {
		t.Fatalf("substop self-check failed: %v\nhub log:\n%s", f, e.Hub.LogTail(3000))
	}
}

// TestSuiteSelfCheck_Guard validates stranger_guard (on / off / per-bot override).
func TestSuiteSelfCheck_Guard(t *testing.T) {
	skipSelfCheck(t)
	on := func(t *testing.T) {
		e := newEnv(t, simkit.RefHubSpec(refhubBin))
		var r simkit.Report
		e.RunGuardON(&r)
		fail(t, &r, e)
	}
	off := func(t *testing.T) {
		g := false
		e := newEnvOpts(t, simkit.RefHubSpec(refhubBin), simkit.Opts{Guard: &g})
		var r simkit.Report
		e.RunGuardOFF(&r)
		fail(t, &r, e)
	}
	ovr := func(t *testing.T) {
		e := newEnvOpts(t, simkit.RefHubSpec(refhubBin), simkit.Opts{BotGuard: map[string]bool{"dsh": false}})
		var r simkit.Report
		e.RunGuardOverride(&r)
		fail(t, &r, e)
	}
	t.Run("on", on)
	t.Run("off", off)
	t.Run("override", ovr)
}

// TestSuiteSelfCheck_Gate validates the dangerous-action approval gate (a)-(h)
// plus the injected-partner bypass and the documented blind spots.
func TestSuiteSelfCheck_Gate(t *testing.T) {
	skipSelfCheck(t)
	run := func(name string, fn func(*simkit.Env, *simkit.Report), opts simkit.Opts) {
		t.Run(name, func(t *testing.T) {
			e := newEnvOpts(t, simkit.RefHubSpec(refhubBin), opts)
			var r simkit.Report
			fn(e, &r)
			fail(t, &r, e)
		})
	}
	run("cards_only_once", (*simkit.Env).RunGateCards, simkit.Opts{})
	run("non_blocking", (*simkit.Env).RunGateNonBlocking, simkit.Opts{})
	run("task_grant", (*simkit.Env).RunGateTaskGrant, simkit.Opts{})
	run("normal_no_prompt", (*simkit.Env).RunGateNormalNoPrompt, simkit.Opts{})
	run("deny", (*simkit.Env).RunGateDeny, simkit.Opts{})
	run("only_sin", (*simkit.Env).RunGateOnlySin, simkit.Opts{})
	run("no_timeout", (*simkit.Env).RunGateNoTimeout, simkit.Opts{})
	run("stop_cancels", (*simkit.Env).RunGateStopCancels, simkit.Opts{})
	run("restart_survives", (*simkit.Env).RunGateRestart, simkit.Opts{})
	run("bypass_injected", (*simkit.Env).RunGateBypassInjected, simkit.Opts{})
	run("extra_class", (*simkit.Env).RunGateExtra, simkit.Opts{Dangerous: map[string]any{"use_defaults": true, "extra_commands": []string{`terraform\s+destroy`}}})
	run("disable_class", (*simkit.Env).RunGateDisable, simkit.Opts{Dangerous: map[string]any{"use_defaults": true, "disable": []string{"git.force"}}})
	run("gaps_documented", (*simkit.Env).RunGaps, simkit.Opts{})
	// (g) ungated partner refuses to start unless ungated_ok
	t.Run("ungated_blocked", func(t *testing.T) {
		e := newEnvOpts(t, simkit.RefHubSpec(refhubBin), simkit.Opts{BypassArg: map[string]string{"claude": "--dangerously-skip-permissions"}})
		var r simkit.Report
		e.RunUngatedBlocked(&r)
		fail(t, &r, e)
	})
	t.Run("ungated_allowed", func(t *testing.T) {
		e := newEnvOpts(t, simkit.RefHubSpec(refhubBin),
			simkit.Opts{BypassArg: map[string]string{"claude": "--dangerously-skip-permissions"}, Ungated: map[string]bool{"claude": true}})
		var r simkit.Report
		e.RunUngatedAllowed(&r)
		fail(t, &r, e)
	})
}

// TestSuiteSelfCheck_Marathon validates the no-turn-cap / no end-of-turn-JSON check.
func TestSuiteSelfCheck_Marathon(t *testing.T) {
	skipSelfCheck(t)
	e := newEnv(t, simkit.RefHubSpec(refhubBin))
	var r simkit.Report
	e.RunMarathon(&r)
	fail(t, &r, e)
}

// TestSuiteSelfCheck_Mutations proves the checks are not vacuous: a stub with a
// guarantee deliberately broken must make the matching check FAIL.
func TestSuiteSelfCheck_Mutations(t *testing.T) {
	skipSelfCheck(t)
	cases := []struct {
		name, breakKey string
		mustFail       []string // the mutation must make at least one of these checks fail
		run            func(*simkit.Env, *simkit.Report)
		opts           simkit.Opts
	}{
		{"no_dedupe", "no_dedupe", []string{"A8", "A6a"}, runMain, simkit.Opts{Long: map[string]string{"codex": "tool"}, Misbehave: []string{"codex"}}},
		{"no_resume", "no_resume", []string{"A9"}, runMain, simkit.Opts{Long: map[string]string{"codex": "tool"}, Misbehave: []string{"codex"}}},
		{"no_eyes", "no_eyes", []string{"A1"}, runMain, simkit.Opts{Long: map[string]string{"codex": "tool"}, Misbehave: []string{"codex"}}},
		{"mutable_done_when", "mutable_done_when", []string{"H3"}, runMain, simkit.Opts{EditDone: true, Long: map[string]string{"codex": "tool"}, Misbehave: []string{"codex"}}},
		{"drop_field", "", []string{"H1"}, runMain, simkit.Opts{DropField: "evidence", Long: map[string]string{"codex": "tool"}, Misbehave: []string{"codex"}}},
		{"no_kill", "no_kill", []string{"B3"}, (*simkit.Env).RunStop, simkit.Opts{Long: map[string]string{"claude": "tick", "codex": "tool", "dsh": "gen"}, Misbehave: []string{"codex"}}},
		{"anyone_stops", "anyone_stops", []string{"B2"}, (*simkit.Env).RunStop, simkit.Opts{Long: map[string]string{"claude": "tick", "codex": "tool", "dsh": "gen"}, Misbehave: []string{"codex"}}},
		{"no_guard", "no_guard", []string{"D1"}, (*simkit.Env).RunGuardON, simkit.Opts{}},
		{"no_overrides", "no_overrides", []string{"D5"}, (*simkit.Env).RunGuardOverride, simkit.Opts{BotGuard: map[string]bool{"dsh": false}}},
		{"no_gate", "no_gate", []string{"E-push-b"}, (*simkit.Env).RunGateCards, simkit.Opts{}},
		{"any_click", "any_click", []string{"E-onlysin"}, (*simkit.Env).RunGateOnlySin, simkit.Opts{}},
		{"gate_timeout", "gate_timeout", []string{"E-notimeout"}, (*simkit.Env).RunGateNoTimeout, simkit.Opts{}},
		{"no_stop_cancel", "no_stop_cancel", []string{"E-stopcancel"}, (*simkit.Env).RunGateStopCancels, simkit.Opts{}},
		{"no_card_update", "no_card_update", []string{"E-restart"}, (*simkit.Env).RunGateRestart, simkit.Opts{}},
		{"ignore_ungated", "ignore_ungated", []string{"E-ungated"}, runUngatedBlocked, simkit.Opts{BypassArg: map[string]string{"claude": "--dangerously-skip-permissions"}}},
		{"turn_cap", "turn_cap", []string{"F1"}, (*simkit.Env).RunMarathon, simkit.Opts{MarathonMS: 11000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mutSem <- struct{}{}
			defer func() { <-mutSem }()
			spec := simkit.RefHubSpec(refhubBin)
			if c.breakKey != "" {
				spec = simkit.RefHubSpec(refhubBin, c.breakKey)
			}
			e := newEnvOpts(t, spec, c.opts)
			var r simkit.Report
			c.run(e, &r)
			failed := r.Failed()
			t.Logf("mutation %s -> failed %v\n%s", c.name, failed, r.String())
			hit := false
			for _, mf := range c.mustFail {
				if slices.Contains(failed, mf) {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("mutation %s should make one of %v fail; failed=%v", c.name, c.mustFail, failed)
			}
		})
	}
}

func runUngatedBlocked(e *simkit.Env, r *simkit.Report) { e.RunUngatedBlocked(r) }
func runMain(e *simkit.Env, r *simkit.Report)           { e.RunMain(r) }

func fail(t *testing.T, r *simkit.Report, e *simkit.Env) {
	t.Helper()
	t.Logf("%s", r.String())
	if f := r.Failed(); len(f) > 0 {
		t.Fatalf("failed checks: %v\nhub log:\n%s", f, e.Hub.LogTail(3000))
	}
}

// mutSem caps how many heavy mutation scenarios run at once (each is a full
// hub + 4 harnesses); unbounded parallelism exhausts the box.
var mutSem = make(chan struct{}, 4)
