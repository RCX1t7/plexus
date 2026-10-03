//go:build !windows

package platform

import (
	"os/exec"
	"syscall"
)

// HideWindow is a no-op outside Windows.
func HideWindow(cmd *exec.Cmd) {}

// PrepareTree puts the child in its own process group (call before Start).
func PrepareTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Tree kills a child process group.
type Tree struct{ pgid int }

// AttachTree records the process group of a started command.
func AttachTree(cmd *exec.Cmd) (*Tree, error) { return &Tree{pgid: cmd.Process.Pid}, nil }

// Kill sends SIGKILL to the whole group.
func (t *Tree) Kill() error {
	if t == nil || t.pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-t.pgid, syscall.SIGKILL)
	t.pgid = 0
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

// OpenBrowser opens url with xdg-open (best effort).
func OpenBrowser(url string) error { return exec.Command("xdg-open", url).Start() }

// HideConsole is a no-op outside Windows.
func HideConsole() {}
