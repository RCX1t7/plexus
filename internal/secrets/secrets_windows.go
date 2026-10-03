//go:build windows

package secrets

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

// Open returns the DPAPI-backed store in dir.
func Open(dir string) (Store, error) {
	return &sealedFile{path: filepath.Join(dir, "secrets.dpapi"), seal: protect, open: unprotect}, nil
}

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procProtect   = crypt32.NewProc("CryptProtectData")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree = kernel32.NewProc("LocalFree")
	entropy       = []byte("plexus-secrets-v1")
)

const cryptprotectUIForbidden = 0x1

type dataBlob struct {
	Size uint32
	Data *byte
}

func blob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func dpapi(proc *syscall.LazyProc, in []byte) ([]byte, error) {
	var out dataBlob
	r, _, e := proc.Call(uintptr(unsafe.Pointer(blob(in))), 0, uintptr(unsafe.Pointer(blob(entropy))),
		0, 0, cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, e
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// protect / unprotect call CryptProtectData / CryptUnprotectData: the blob
// is bound to the current Windows user.
func protect(plain []byte) ([]byte, error)    { return dpapi(procProtect, plain) }
func unprotect(sealed []byte) ([]byte, error) { return dpapi(procUnprotect, sealed) }
