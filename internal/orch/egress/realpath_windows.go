//go:build windows

package egress

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// Why this file exists (review 2026-07-25): filepath.EvalSymlinks is NOT a link
// guard on Windows. A DIRECTORY JUNCTION (`mklink /J`) is reported by Go as
// os.ModeIrregular, not os.ModeSymlink, so EvalSymlinks returns the path
// UNCHANGED with a nil error — the guard degenerated into `abs = abs` and a
// junction placed inside an allowlisted repo re-exported its target.
//
// The asymmetry is what made it dangerous: `mklink /D` (a real symlink) requires
// elevation, while `mklink /J` succeeds for any unprivileged user — so the only
// case the old guard caught was the one that needs admin, and the one anybody
// can create walked straight through. Package managers, build scripts and
// Explorer create junctions routinely, so this is an accident, not just an
// attack.
//
// GetFinalPathNameByHandleW asks the kernel what the handle actually points at,
// which resolves symlinks, junctions and mount points alike. Implemented over
// stdlib syscall on purpose: this module has ZERO dependencies and keeps them.
var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathName = kernel32.NewProc("GetFinalPathNameByHandleW")
)

const (
	// Required to open a DIRECTORY handle at all.
	fileFlagBackupSemantics = 0x02000000
	// VOLUME_NAME_DOS: return a drive-letter path rather than a volume GUID.
	volumeNameDOS = 0x0
)

// realPath returns the canonical on-disk path p resolves to, following symlinks,
// junctions and mount points. It returns a not-exist error when p is absent, so
// callers can distinguish "cannot be resolved" from "not there yet".
func realPath(p string) (string, error) {
	u16, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	// Zero desired-access: metadata only. No read, so this works on files whose
	// contents we are not permitted to read, and never locks anything.
	h, err := syscall.CreateFile(u16, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, fileFlagBackupSemantics, 0)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)

	buf := make([]uint16, 512)
	for {
		n, _, errno := procGetFinalPathName.Call(uintptr(h),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), volumeNameDOS)
		if n == 0 {
			return "", errno
		}
		if int(n) < len(buf) {
			break
		}
		if int(n) > 1<<16 { // refuse to grow without bound
			return "", fmt.Errorf("canonical path for %q exceeds %d UTF-16 units", p, 1<<16)
		}
		buf = make([]uint16, n+1)
	}
	s := syscall.UTF16ToString(buf)
	// The call returns the extended-length form; normalize it back so the
	// containment test compares like with like.
	if rest, ok := strings.CutPrefix(s, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(s, `\\?\`), nil
}
