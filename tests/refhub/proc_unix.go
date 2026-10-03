//go:build !windows

package refhub

import "syscall"

// killPGID kills a whole process group (the harness ran with Setpgid, so this
// reaps its children and grandchildren too). Stands in for Windows
// TerminateJobObject / KILL_ON_JOB_CLOSE in the reference stub.
func killPGID(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
