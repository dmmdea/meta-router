//go:build !windows

package egress

import "path/filepath"

// realPath returns the canonical on-disk path p resolves to. On POSIX
// EvalSymlinks IS sufficient — there is no junction/reparse-point class that
// evades it, which is the whole reason the Windows implementation is separate.
func realPath(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
