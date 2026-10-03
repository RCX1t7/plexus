// Command plexus connects the coding-agent harnesses on this computer to
// Slack as partners, one Slack app per partner.
//
//	plexus run [--config DIR] [--setup] [--hidden]   run all partners (default)
//	plexus setup [--config DIR]                     run and open the setup page
//	plexus detect                                   print detected harnesses as JSON
//	plexus stop <task-id> [--config DIR]            stop a task tree
//	plexus install-task [--exe PATH]                register the logon task (Windows)
//	plexus version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	_ "github.com/RCX1t7/plexus/internal/adapters/all"
	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/platform"
	"github.com/RCX1t7/plexus/internal/redact"
	"github.com/RCX1t7/plexus/internal/store"
	"github.com/RCX1t7/plexus/internal/supervisor"
	"github.com/RCX1t7/plexus/scripts"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "plexus:", redact.String(err.Error()))
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config directory (or path to its config.json)")
	setupFlag := fs.Bool("setup", false, "open the setup page")
	hidden := fs.Bool("hidden", false, "hide the console window (Task Scheduler)")
	exeFlag := fs.String("exe", "", "plexus.exe path for install-task")
	// Flags may come before or after positional arguments.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	cfgDir, dataDir, err := dirs(*cfgFlag)
	if err != nil {
		return err
	}
	switch cmd {
	case "run", "setup":
		if *hidden {
			platform.HideConsole()
		}
		return serve(cfgDir, dataDir, *setupFlag || cmd == "setup")
	case "detect":
		cfg, err := config.Load(cfgDir)
		if err != nil {
			return err
		}
		supervisor.RegisterConfigACP(cfg, nopLogger())
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(harness.DetectAll(context.Background(), harness.OSEnv()))
	case "stop", "revoke":
		if len(pos) != 1 {
			return errors.New("usage: plexus stop <task-id>")
		}
		id := pos[0]
		n, err := supervisor.RemoteStop(dataDir, id)
		if err == nil {
			fmt.Printf("stopped %s (%d running sessions ended)\n", id, n)
			return nil
		}
		if !errors.Is(err, supervisor.ErrNoHub) {
			return err
		}
		st, err := store.Open(filepath.Join(dataDir, "plexus.db"))
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.Revoke(id, "cli"); err != nil {
			return err
		}
		fmt.Println("stopped", id, "(no hub running; it will not start new turns on this task)")
		return nil
	case "install-task":
		return installTask(*exeFlag)
	case "version":
		fmt.Println("plexus", version, runtime.GOOS+"/"+runtime.GOARCH)
		return nil
	}
	return fmt.Errorf("unknown command %q (run, setup, detect, stop, install-task, version)", cmd)
}

// dirs resolves the config and data directories. --config puts both in
// one directory (like PLEXUS_HOME).
func dirs(flagVal string) (string, string, error) {
	if flagVal == "" {
		return platform.ConfigDir(), platform.DataDir(), nil
	}
	d := flagVal
	if filepath.Ext(d) == ".json" {
		if filepath.Base(d) != "config.json" {
			return "", "", errors.New("--config must be a directory or a file named config.json")
		}
		d = filepath.Dir(d)
	}
	abs, err := filepath.Abs(d)
	return abs, abs, err
}

func serve(cfgDir, dataDir string, openSetup bool) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	var out io.Writer = os.Stderr
	if f, err := openLog(dataDir); err == nil {
		defer f.Close()
		out = io.MultiWriter(os.Stderr, f)
	}
	lw := &redact.Writer{W: out}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hub, err := supervisor.Open(ctx, cfgDir, dataDir, lw)
	if err != nil {
		return err
	}
	return hub.Run(ctx, openSetup)
}

// openLog appends to plexus.log, starting over when it passes 10 MB.
func openLog(dir string) (*os.File, error) {
	p := filepath.Join(dir, "plexus.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

func installTask(exe string) error {
	if runtime.GOOS != "windows" {
		return errors.New("install-task is for Windows (Task Scheduler)")
	}
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp("", "plexus-install-*.ps1")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(scripts.InstallTask); err != nil {
		f.Close()
		return err
	}
	f.Close()
	c := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", f.Name(), "-Exe", exe)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
