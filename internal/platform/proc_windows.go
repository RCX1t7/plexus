//go:build windows

package platform

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// LazySystemDLL loads only from System32 (no DLL search-path hijack).
	modkernel32          = windows.NewLazySystemDLL("kernel32.dll")
	moduser32            = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow = modkernel32.NewProc("GetConsoleWindow")
	procShowWindow       = moduser32.NewProc("ShowWindow")
)

// HideWindow stops child console windows from flashing on screen.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}

// PrepareTree must be called before cmd.Start.
func PrepareTree(cmd *exec.Cmd) { HideWindow(cmd) }

// Tree kills a child process and all of its descendants.
type Tree struct{ job windows.Handle }

// AttachTree puts the started process into a Job Object with
// KILL_ON_JOB_CLOSE (no breakaway), so the whole tree dies on Kill and also
// if Plexus itself is killed. Descendants spawned before the assignment
// could escape; harness CLIs spawn workers later.
func AttachTree(cmd *exec.Cmd) (*Tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &Tree{job: job}, nil
}

// Kill terminates every process in the job.
func (t *Tree) Kill() error {
	if t == nil || t.job == 0 {
		return nil
	}
	err := windows.TerminateJobObject(t.job, 1)
	windows.CloseHandle(t.job)
	t.job = 0
	return err
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// HideConsole hides this process' console window (used by `run --hidden`
// when started by Task Scheduler).
func HideConsole() {
	if procGetConsoleWindow.Find() != nil || procShowWindow.Find() != nil {
		return
	}
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		_, _, _ = procShowWindow.Call(hwnd, windows.SW_HIDE)
	}
}

// ProcessAlive reports whether a process with this pid is still running.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false // gone, or not ours to open (treated as gone)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
