// Package exe names test-built binaries portably.
package exe

import "runtime"

// Name returns path with ".exe" appended on Windows. `go build -o p` writes
// exactly p, and Windows cannot exec a file without an executable
// extension ("executable file not found in %PATH%"), so every test that
// builds a binary and then runs it must name it through here.
func Name(path string) string {
	if runtime.GOOS == "windows" {
		return path + ".exe"
	}
	return path
}
