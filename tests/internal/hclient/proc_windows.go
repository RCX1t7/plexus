//go:build windows

package hclient

import (
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func setpgid(*exec.Cmd) {} // the tree is a Job Object, attached after Start

// jobs maps a started *exec.Cmd to the Job Object holding its tree.
var jobs sync.Map

// attachTree puts a started harness into a KILL_ON_JOB_CLOSE Job Object,
// the Windows counterpart of the Unix process group: killTree then ends the
// fake harness's child and grandchild too. Without it a surviving
// grandchild keeps the test's TempDir as its working directory and Windows
// refuses to delete it ("being used by another process"). The fake harness
// spawns its long-mode children only on a later prompt, after this runs.
func attachTree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return
	}
	jobs.Store(c, job)
}

// basicAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type basicAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

func activeProcesses(job windows.Handle) (uint32, error) {
	var a basicAccounting
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&a)), uint32(unsafe.Sizeof(a)), nil)
	return a.ActiveProcesses, err
}

// killTree terminates the whole job and waits (bounded) until no process
// is left in it, so the caller can delete the working directory right away.
func killTree(c *exec.Cmd) {
	v, ok := jobs.LoadAndDelete(c)
	if !ok {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		return
	}
	job := v.(windows.Handle)
	defer windows.CloseHandle(job)
	_ = windows.TerminateJobObject(job, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, err := activeProcesses(job); err != nil || n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}
