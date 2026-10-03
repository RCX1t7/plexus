//go:build !windows

package hclient

import (
	"os/exec"
	"syscall"
)

func setpgid(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killTree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	if c.SysProcAttr != nil && c.SysProcAttr.Setpgid {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		return
	}
	_ = c.Process.Kill()
}
