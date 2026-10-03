//go:build linux

package simkit

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Proc is one row of the process table.
type Proc struct {
	PID, PPID, PGID int
	State           string
	Cmdline         string
	UTime, STime    uint64 // clock ticks
	RSSKiB          int64
}

// ListProcs reads /proc. Processes that disappear mid-scan are skipped.
func ListProcs() []Proc {
	ents, _ := os.ReadDir("/proc")
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ok := ReadProc(pid); ok {
			out = append(out, p)
		}
	}
	return out
}

// ReadProc reads a single /proc/<pid>.
func ReadProc(pid int) (Proc, bool) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Proc{}, false
	}
	// comm may contain spaces/parens: split after the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return Proc{}, false
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 22 {
		return Proc{}, false
	}
	p := Proc{PID: pid, State: f[0]}
	p.PPID, _ = strconv.Atoi(f[1])
	p.PGID, _ = strconv.Atoi(f[2])
	p.UTime, _ = strconv.ParseUint(f[11], 10, 64)
	p.STime, _ = strconv.ParseUint(f[12], 10, 64)
	if cl, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.Cmdline = strings.TrimSpace(strings.ReplaceAll(string(cl), "\x00", " "))
	}
	if st, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		for _, line := range strings.Split(string(st), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fs := strings.Fields(line)
				if len(fs) >= 2 {
					p.RSSKiB, _ = strconv.ParseInt(fs[1], 10, 64)
				}
			}
		}
	}
	return p, true
}

// Alive reports whether pid exists and is not a zombie.
func Alive(pid int) bool {
	p, ok := ReadProc(pid)
	return ok && p.State != "Z"
}

// ProcsWithEnv returns live (non-zombie) processes whose environment contains
// key=value. Child processes inherit the marker, so this finds the whole task
// tree even after re-parenting to init (unless a process scrubs its env).
func ProcsWithEnv(key, value string) []Proc {
	needle := []byte(key + "=" + value)
	var out []Proc
	for _, p := range ListProcs() {
		if p.State == "Z" {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p.PID), "environ"))
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(env, []byte{0}) {
			if bytes.Equal(kv, needle) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Descendants returns all live descendants of root (by PPID chain).
func Descendants(root int) []Proc {
	all := ListProcs()
	kids := map[int][]Proc{}
	for _, p := range all {
		kids[p.PPID] = append(kids[p.PPID], p)
	}
	var out []Proc
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			if k.State != "Z" {
				out = append(out, k)
			}
			queue = append(queue, k.PID)
		}
	}
	return out
}

// ClockTicks is USER_HZ (100 on every mainstream Linux).
const ClockTicks = 100
