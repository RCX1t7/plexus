//go:build windows

package hclient

import "os/exec"

func setpgid(*exec.Cmd) {} // refhub is Linux-only; Plexus uses a Job Object on Windows

func killTree(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}
