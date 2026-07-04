package index

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RotateBackup implements MR-16 backup retention: before an index overwrite,
// copy the existing index to exactly ONE dated backup
// (<path>.bak-YYYYMMDD-HHMMSS) and prune every other <path>.bak* file —
// including older dated backups and any hand-made ones (the previous
// convention was ad-hoc manual copies that accumulated forever).
// Returns the new backup path, or "" when there is no index to back up
// (in which case nothing is pruned either).
func RotateBackup(path string) (string, error) {
	// Normalize first: Glob returns OS-normalized separators, and the prune
	// below compares paths as strings — a mixed-separator input (e.g.
	// C:\x/y\index.json from a shell) would otherwise never equal its own
	// glob match and the freshly written backup would prune itself.
	path = filepath.Clean(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // nothing to back up
		}
		return "", err
	}
	bak := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(bak, data, 0o644); err != nil {
		return "", err
	}
	// Prune everything else matching <path>.bak* — keep exactly one.
	matches, err := filepath.Glob(path + ".bak*")
	if err != nil {
		return bak, nil // glob pattern is static; treat as nothing to prune
	}
	for _, m := range matches {
		if m == bak {
			continue
		}
		if rerr := os.Remove(m); rerr != nil {
			// Best-effort prune: a locked leftover shouldn't fail the refresh.
			fmt.Fprintf(os.Stderr, "warning: could not prune old backup %s: %v\n", m, rerr)
		}
	}
	return bak, nil
}
