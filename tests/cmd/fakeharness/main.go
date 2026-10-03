// Command fakeharness is the scripted fake harness (see package fakeharness).
//
//	fakeharness [--proto claude|codex|dsh-bridge|acp] [--sim-role R] <native argv as Plexus builds it>
//	fakeharness --version
//	fakeharness child --sleep 600 [--grandchild] [--ignore-term]   (long-mode child process)
//	fakeharness count FILE                                          (codex's independent count)
//
// Without --proto the protocol is detected from the native argv
// (`app-server`, `--input-format stream-json`, `--profile plexus`, `--acp`).
// The role comes from --sim-role, $HH_SIM_ROLE, or the executable name
// (fake-claude, claude, ...). Everything else comes from the environment,
// which Plexus passes through to harnesses (ARCHITECTURE answer #17):
//
//	HH_SIM_STATE  HH_SIM_LEDGER  HH_SIM_PROMPTLOG  HH_SIM_OWNER  HH_SIM_PEERS=claude=U..,codex=U..
//	HH_SIM_TOOL_DELAY  HH_SIM_MISBEHAVE=claude,dsh  HH_SIM_LONG=claude=tick,codex=tool,dsh=gen
//	HH_SIM_TICK  HH_SIM_MARATHON_TURN  HH_SIM_HANDOFF_DROP=<field>  HH_SIM_EDIT_DONE_WHEN=1
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RCX1t7/plexus/tests/fakeharness"
)

var protoAlias = map[string]string{
	"claude": fakeharness.ProtoClaude, "codex": fakeharness.ProtoCodex, "dsh-bridge": fakeharness.ProtoDSH, "dsh": fakeharness.ProtoDSH,
	"acp": fakeharness.ProtoACP, fakeharness.ProtoClaude: fakeharness.ProtoClaude, fakeharness.ProtoCodex: fakeharness.ProtoCodex,
	fakeharness.ProtoDSH: fakeharness.ProtoDSH,
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "child":
			child(args[1:])
			return
		case "count":
			data, err := os.ReadFile(args[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			_ = json.NewEncoder(os.Stdout).Encode(fakeharness.CountBytes(data))
			return
		case "--version", "-v", "version":
			fmt.Println("0.0.0-fake (plexus acceptance fakeharness)")
			return
		}
	}
	var proto, role, resume string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--proto":
			proto = val()
		case strings.HasPrefix(a, "--proto="):
			proto = strings.TrimPrefix(a, "--proto=")
		case a == "--sim-role":
			role = val()
		case strings.HasPrefix(a, "--sim-role="):
			role = strings.TrimPrefix(a, "--sim-role=")
		case a == "--resume" || a == "-r":
			resume = val()
		case strings.HasPrefix(a, "--resume="):
			resume = strings.TrimPrefix(a, "--resume=")
		default:
			rest = append(rest, a)
		}
	}
	if proto == "" {
		proto = fakeharness.DetectProto(rest)
	} else {
		proto = protoAlias[proto]
	}
	if proto == "" {
		fmt.Fprintln(os.Stderr, "fakeharness: cannot tell the protocol from argv; use --proto claude|codex|dsh-bridge|acp")
		os.Exit(2)
	}
	if role == "" {
		role = os.Getenv("HH_SIM_ROLE")
	}
	if role == "" {
		base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
		role = strings.TrimPrefix(base, "fake-")
	}
	cfg := fakeharness.Config{Role: role, Ledger: os.Getenv("HH_SIM_LEDGER"), PromptLog: os.Getenv("HH_SIM_PROMPTLOG"),
		Owner: env("HH_SIM_OWNER", "U0SIN"), Peers: kv(os.Getenv("HH_SIM_PEERS")), DropField: os.Getenv("HH_SIM_HANDOFF_DROP"), EditDoneWhen: os.Getenv("HH_SIM_EDIT_DONE_WHEN") == "1",
		Long: kv(os.Getenv("HH_SIM_LONG"))[role]}
	cfg.Workspace, _ = os.Getwd()
	cfg.StateDir = env("HH_SIM_STATE", filepath.Join(cfg.Workspace, ".fh-state"))
	cfg.ToolDelay, _ = time.ParseDuration(os.Getenv("HH_SIM_TOOL_DELAY"))
	cfg.TickEvery, _ = time.ParseDuration(os.Getenv("HH_SIM_TICK"))
	cfg.MarathonTurn, _ = time.ParseDuration(os.Getenv("HH_SIM_MARATHON_TURN"))
	for _, r := range strings.Split(os.Getenv("HH_SIM_MISBEHAVE"), ",") {
		cfg.Misbehave = cfg.Misbehave || strings.TrimSpace(r) == role
	}
	cfg.Exe, _ = os.Executable()
	// Writes to a dead hub return EPIPE instead of killing us with SIGPIPE.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	b := fakeharness.NewBrain(cfg)
	bypass := false
	for i, a := range rest {
		if a == "--dangerously-skip-permissions" || a == "--permission-mode=bypassPermissions" ||
			(a == "--permission-mode" && i+1 < len(rest) && rest[i+1] == "bypassPermissions") {
			bypass = true
		}
	}
	d, err := fakeharness.NewDriver(proto, b, os.Stdout, fakeharness.DriverOptions{ResumeID: resume, Bypass: bypass})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := d.Serve(os.Stdin); err != nil {
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func kv(s string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			m[k] = v
		}
	}
	return m
}

func child(args []string) {
	fs := flag.NewFlagSet("child", flag.ExitOnError)
	sleep := fs.Int("sleep", 600, "seconds")
	grand := fs.Bool("grandchild", false, "spawn a grandchild")
	ignore := fs.Bool("ignore-term", false, "ignore SIGTERM")
	_ = fs.Parse(args)
	var gc *exec.Cmd
	if *grand {
		exe, _ := os.Executable()
		a := []string{"child", "--sleep", fmt.Sprint(*sleep)}
		if *ignore {
			a = append(a, "--ignore-term")
		}
		gc = exec.Command(exe, a...)
		_ = gc.Start()
	}
	sig := make(chan os.Signal, 1)
	if *ignore {
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	} else {
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	}
	select {
	case <-sig:
		if gc != nil && gc.Process != nil {
			_ = gc.Process.Signal(syscall.SIGTERM)
			_, _ = gc.Process.Wait()
		}
	case <-time.After(time.Duration(*sleep) * time.Second):
	}
}
