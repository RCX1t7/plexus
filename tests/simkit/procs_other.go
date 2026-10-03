//go:build !linux

package simkit

// Process-table checks are Linux-only in the scripted simulation; on Windows
// use the PowerShell checklist in ACCEPTANCE.md (Get-CimInstance Win32_Process).

type Proc struct {
	PID, PPID, PGID int
	State           string
	Cmdline         string
	UTime, STime    uint64
	RSSKiB          int64
}

func ListProcs() []Proc                     { return nil }
func ReadProc(pid int) (Proc, bool)         { return Proc{}, false }
func Alive(pid int) bool                    { return false }
func ProcsWithEnv(key, value string) []Proc { return nil }
func Descendants(root int) []Proc           { return nil }

const ClockTicks = 100
