package simkit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/RCX1t7/plexus/tests/fakeslack"
)

// HubSpec says which hub binary the scenario drives.
type HubSpec struct {
	Name     string   // "plexus" or "refhub"
	Bin      string   // executable
	ExtraEnv []string // e.g. REFHUB_BREAK=no_dedupe (refhub only)
	// ConfigTemplate optionally overrides the generated config.json
	// (text/template over ConfigVars; PLEXUS_CONFIG_TEMPLATE).
	ConfigTemplate string
}

// HubSpecFromEnv returns the real Plexus spec, or a skip reason.
//
//	PLEXUS_BIN              path to a built plexus binary (required)
//	PLEXUS_CONFIG_TEMPLATE  optional config.json template file
func HubSpecFromEnv() (HubSpec, string) {
	bin := os.Getenv("PLEXUS_BIN")
	if bin == "" {
		return HubSpec{}, "PLEXUS_BIN is not set: build Plexus (cmd/plexus is still empty in the 17:30 skeleton) and export PLEXUS_BIN=/path/to/plexus"
	}
	if _, err := os.Stat(bin); err != nil {
		return HubSpec{}, "PLEXUS_BIN=" + bin + ": " + err.Error()
	}
	spec := HubSpec{Name: "plexus", Bin: bin}
	if p := os.Getenv("PLEXUS_CONFIG_TEMPLATE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return HubSpec{}, "PLEXUS_CONFIG_TEMPLATE: " + err.Error()
		}
		spec.ConfigTemplate = string(b)
	}
	return spec, ""
}

// RefHubSpec is the reference stub hub used to validate this suite.
func RefHubSpec(bin string, breaks ...string) HubSpec {
	s := HubSpec{Name: "refhub", Bin: bin}
	if len(breaks) > 0 {
		s.ExtraEnv = append(s.ExtraEnv, "REFHUB_BREAK="+strings.Join(breaks, ","))
	}
	return s
}

// BotConfig is one partner in the generated config.
type BotConfig struct {
	Name, Harness, UserID, AppID, Exe string
	Args                              []string
	StrangerGuard                     *bool // per-bot override (nil = inherit)
	Ungated                           bool  // bots[].ungated_ok
}

// ConfigVars feed the config.json generator / template.
type ConfigVars struct {
	Owner         string
	StrangerGuard bool
	SlackAPIURL   string
	SetupPort     int
	Workdir       string
	FakeACP       string // fakeharness path used by the acp_harnesses entry
	Bots          []BotConfig
	// Dangerous is the dangerous_actions object; nil -> {use_defaults:true}.
	Dangerous map[string]any
}

// ConfigJSON renders config.json with exactly the agreed keys (answer #14 +
// coordinator): owner, stranger_guard, bots[]{name, display_name, harness,
// enabled, workdir, user_id, exe, args, app_id, stranger_guard?},
// acp_harnesses, setup_port, slack_api_url. "danger_gate" is a PROPOSAL
// (Sin's approval gate; no key in REQUIREMENTS/ARCHITECTURE yet).
func ConfigJSON(v ConfigVars, tmpl string) ([]byte, error) {
	if tmpl != "" {
		t, err := template.New("cfg").Parse(tmpl)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		err = t.Execute(&buf, v)
		return buf.Bytes(), err
	}
	var bots []any
	for _, b := range v.Bots {
		m := map[string]any{"name": b.Name, "display_name": "Plexus " + b.Name, "harness": b.Harness, "enabled": true,
			"workdir": v.Workdir, "user_id": b.UserID, "exe": b.Exe, "args": b.Args, "app_id": b.AppID}
		if b.StrangerGuard != nil {
			m["stranger_guard"] = *b.StrangerGuard
		}
		if b.Ungated {
			m["ungated_ok"] = true
		}
		bots = append(bots, m)
	}
	danger := v.Dangerous
	if danger == nil {
		danger = map[string]any{"use_defaults": true}
	}
	cfg := map[string]any{"owner": v.Owner, "stranger_guard": v.StrangerGuard, "bots": bots, "setup_port": v.SetupPort,
		"slack_api_url": v.SlackAPIURL, "dangerous_actions": danger,
		"acp_harnesses": []any{map[string]any{"name": "fake_acp", "executables": []string{v.FakeACP}, "args": []string{"--acp"},
			"version_args": []string{"--version"}}}}
	return json.MarshalIndent(cfg, "", "  ")
}

// WriteHome writes config.json and the token sources into PLEXUS_HOME:
// secrets.dev.json (skeleton: PLEXUS_INSECURE_DEV_SECRETS=1 on Linux) and
// returns the env overrides PLEXUS_BOT_TOKEN_<NAME>/PLEXUS_APP_TOKEN_<NAME>
// (answer #14).
func WriteHome(home string, cfg []byte, bots []fakeslack.BotIdentity) ([]string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), cfg, 0o600); err != nil {
		return nil, err
	}
	sec := map[string]string{}
	env := []string{"PLEXUS_HOME=" + home, "PLEXUS_INSECURE_DEV_SECRETS=1"}
	for _, b := range bots {
		sec["bot/"+b.Name+"/bot_token"] = b.Token
		sec["bot/"+b.Name+"/app_token"] = b.AppToken
		up := strings.ToUpper(b.Name)
		env = append(env, "PLEXUS_BOT_TOKEN_"+up+"="+b.Token, "PLEXUS_APP_TOKEN_"+up+"="+b.AppToken)
	}
	data, _ := json.Marshal(sec)
	return env, os.WriteFile(filepath.Join(home, "secrets.dev.json"), data, 0o600)
}

// ReadyRE is the readiness line (`plexus ready bots=<n>`). The coordinator
// said stderr, ARCHITECTURE §4.3 / answer #15 say stdout: both are watched
// and the stream is reported.
var ReadyRE = regexp.MustCompile(`plexus ready bots=(\d+)`)

// HubProc is one running hub process generation.
type HubProc struct {
	Spec   HubSpec
	Env    []string
	LogDir string
	Gen    int

	cmd       *exec.Cmd
	exited    chan struct{}
	StartedAt time.Time
	mu        sync.Mutex
	ready     chan struct{}
	readyN    int
	readyOn   string
	readyAt   time.Time
}

// Start launches `<bin> run` (a new generation).
func (h *HubProc) Start() error {
	h.Gen++
	cmd := exec.Command(h.Spec.Bin, "run")
	cmd.Env = append(append(os.Environ(), h.Env...), h.Spec.ExtraEnv...)
	cmd.SysProcAttr = sysProcAttr()
	logf, err := os.Create(filepath.Join(h.LogDir, fmt.Sprintf("hub-gen%d.log", h.Gen)))
	if err != nil {
		return err
	}
	so, _ := cmd.StdoutPipe()
	se, _ := cmd.StderrPipe()
	h.mu.Lock()
	h.ready, h.readyN, h.readyOn = make(chan struct{}), 0, ""
	h.mu.Unlock()
	h.StartedAt = time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	var lmu sync.Mutex
	var wg sync.WaitGroup
	watch := func(r io.Reader, stream string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			lmu.Lock()
			fmt.Fprintf(logf, "[%s] %s\n", stream, line)
			lmu.Unlock()
			if m := ReadyRE.FindStringSubmatch(line); m != nil {
				h.mu.Lock()
				if h.readyOn == "" {
					h.readyN, _ = strconv.Atoi(m[1])
					h.readyOn, h.readyAt = stream, time.Now()
					close(h.ready)
				}
				h.mu.Unlock()
			}
		}
	}
	wg.Add(2)
	go watch(so, "stdout")
	go watch(se, "stderr")
	h.cmd = cmd
	h.exited = make(chan struct{})
	ex := h.exited
	go func() { wg.Wait(); _ = cmd.Wait(); logf.Close(); close(ex) }()
	return nil
}

// WaitReady waits for the ready line: (n, stream, time since start, ok).
func (h *HubProc) WaitReady(timeout time.Duration) (int, string, time.Duration, bool) {
	h.mu.Lock()
	ch := h.ready
	h.mu.Unlock()
	select {
	case <-ch:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.readyN, h.readyOn, h.readyAt.Sub(h.StartedAt), true
	case <-h.exited:
		return 0, "", 0, false
	case <-time.After(timeout):
		return 0, "", 0, false
	}
}

// PID of the current generation (0 if not started).
func (h *HubProc) PID() int {
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// Running reports whether the current generation is still alive.
func (h *HubProc) Running() bool {
	if h.exited == nil {
		return false
	}
	select {
	case <-h.exited:
		return false
	default:
		return true
	}
}

// Kill SIGKILLs the hub process only (crash simulation; what happens to its
// harness processes is part of what the scenario checks).
func (h *HubProc) Kill() {
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		// Wait for the stdio readers to drain, but do not block forever: an
		// orphaned harness child can hold the inherited pipe open past the
		// hub's own death.
		select {
		case <-h.exited:
		case <-time.After(5 * time.Second):
		}
	}
}

// Terminate asks the hub to exit (SIGTERM ~ Ctrl+C), then SIGKILLs.
func (h *HubProc) Terminate(timeout time.Duration) {
	if !h.Running() {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.exited:
	case <-time.After(timeout):
		h.Kill()
	}
}

// CLI runs `<bin> args...` with the hub's environment (e.g. `stop <task-id>`).
func (h *HubProc) CLI(timeout time.Duration, args ...string) (string, int, error) {
	cmd := exec.Command(h.Spec.Bin, args...)
	cmd.Env = append(append(os.Environ(), h.Env...), h.Spec.ExtraEnv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			return out.String(), -1, err
		}
		return out.String(), code, nil
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return out.String(), -1, fmt.Errorf("timeout")
	}
}

// LogTail returns the last n bytes of the current generation's log.
func (h *HubProc) LogTail(n int) string {
	b, _ := os.ReadFile(filepath.Join(h.LogDir, fmt.Sprintf("hub-gen%d.log", h.Gen)))
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
