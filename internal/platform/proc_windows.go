//go:build windows

package platform

import (
	"errors"
	"os/exec"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	user32                 = syscall.NewLazyDLL("user32.dll")
	procCreateJobObject    = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject = kernel32.NewProc("TerminateJobObject")
	procGetConsoleWindow   = kernel32.NewProc("GetConsoleWindow")
	procShowWindow         = user32.NewProc("ShowWindow")
	errJob                 = errors.New("job object call failed")
)

// HideWindow stops child console windows from flashing on screen.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}

// PrepareTree must be called before cmd.Start.
func PrepareTree(cmd *exec.Cmd) { HideWindow(cmd) }

// Tree kills a child process and all of its descendants.
type Tree struct{ job syscall.Handle }

// Layouts of JOBOBJECT_BASIC_LIMIT_INFORMATION / _EXTENDED_ (winnt.h).
type ioCounters struct{ R, W, O, RB, WB, OB uint64 }

type basicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type extendedLimit struct {
	Basic                 basicLimit
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

// AttachTree puts the started process into a Job Object with
// KILL_ON_JOB_CLOSE (no breakaway), so the whole tree dies on Kill and also
// if Plexus itself is killed. stdlib syscall only. Descendants spawned
// before the assignment could escape; harness CLIs spawn workers later.
func AttachTree(cmd *exec.Cmd) (*Tree, error) {
	r, _, e := procCreateJobObject.Call(0, 0)
	if r == 0 {
		return nil, e
	}
	job := syscall.Handle(r)
	info := extendedLimit{Basic: basicLimit{LimitFlags: jobObjectLimitKillOnJobClose}}
	if r, _, e := procSetInformationJob.Call(uintptr(job), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(job)
		return nil, e
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		syscall.CloseHandle(job)
		return nil, err
	}
	defer syscall.CloseHandle(h)
	if r, _, e := procAssignProcessToJob.Call(uintptr(job), uintptr(h)); r == 0 {
		syscall.CloseHandle(job)
		if e == nil {
			e = errJob
		}
		return nil, e
	}
	return &Tree{job: job}, nil
}

// Kill terminates every process in the job.
func (t *Tree) Kill() error {
	if t == nil || t.job == 0 {
		return nil
	}
	r, _, e := procTerminateJobObject.Call(uintptr(t.job), 1)
	syscall.CloseHandle(t.job)
	t.job = 0
	if r == 0 {
		return e
	}
	return nil
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// HideConsole hides this process' console window (used by `run --hidden`
// when started by Task Scheduler).
func HideConsole() {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		_, _, _ = procShowWindow.Call(hwnd, 0) // SW_HIDE
	}
}

var (
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
)

// ProcessAlive reports whether a process with this pid is still running.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const queryLimited, stillActive = 0x1000, 259
	h, _, _ := procOpenProcess.Call(queryLimited, 0, uintptr(pid))
	if h == 0 {
		return false // gone, or not ours to open (treated as gone)
	}
	defer procCloseHandle.Call(h)
	var code uint32
	if ok, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code))); ok == 0 {
		return false
	}
	return code == stillActive
}
