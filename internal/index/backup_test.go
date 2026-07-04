package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateBackup_KeepsExactlyOneDatedBak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, []byte(`{"v":"current"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-existing clutter: an old dated backup and a hand-made one.
	for _, old := range []string{path + ".bak-20200101-000000", path + ".bak-manual-preinstall"} {
		if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bak, err := RotateBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if bak == "" || !strings.HasPrefix(filepath.Base(bak), "index.json.bak-") {
		t.Fatalf("bad backup path %q", bak)
	}
	data, err := os.ReadFile(bak)
	if err != nil || string(data) != `{"v":"current"}` {
		t.Fatalf("backup content wrong: %q err=%v", data, err)
	}
	matches, _ := filepath.Glob(path + ".bak*")
	if len(matches) != 1 || matches[0] != bak {
		t.Fatalf("must keep exactly one .bak, got %v", matches)
	}
	// The live index itself is untouched.
	if cur, _ := os.ReadFile(path); string(cur) != `{"v":"current"}` {
		t.Fatalf("live index modified: %q", cur)
	}
}

// Regression: a mixed-separator path (as produced by POSIX shells on
// Windows: C:\dir/sub/index.json) must not make the prune delete the backup
// it just wrote — Glob returns normalized paths, so the comparison has to
// happen on cleaned paths.
func TestRotateBackup_MixedSeparatorsKeepBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mixed := strings.Replace(path, string(os.PathSeparator), "/", 1) // one foreign separator
	bak, err := RotateBackup(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if bak == "" {
		t.Fatal("expected a backup")
	}
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("backup pruned itself on mixed-separator input: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Clean(path) + ".bak*")
	if len(matches) != 1 {
		t.Fatalf("want exactly one bak, got %v", matches)
	}
}

func TestRotateBackup_NoIndexIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	// A stray old bak with no live index: nothing to back up, nothing pruned.
	stray := path + ".bak-old"
	if err := os.WriteFile(stray, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	bak, err := RotateBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if bak != "" {
		t.Fatalf("expected no backup, got %q", bak)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("no-op must not prune existing baks: %v", err)
	}
}
