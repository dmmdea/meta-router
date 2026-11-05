package index

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A truncated index.json once failed every hook run for about 40 hours.
// LoadFast must recover from the sidecar, or from a backup when the sidecar is
// gone.
func TestLoadFast_TornJSONRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	idx := syntheticIndex(5, 8)
	if err := idx.Save(path); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak-20200101-000000", good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	for _, withSidecar := range []bool{true, false} {
		if !withSidecar {
			os.Remove(BinPath(path))
		}
		got, err := LoadFast(path)
		if err != nil || len(got.Entries) != 5 {
			t.Fatalf("sidecar=%v: torn index.json not recovered: %v", withSidecar, err)
		}
	}
}

// Concurrent refreshes shared one fixed temp name and 15.6% of them failed
// (30 days measured). Every save must land and leave a parseable file.
func TestSave_ConcurrentSavesAllSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	idx := syntheticIndex(20, 16)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- idx.Save(path)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Save failed: %v", err)
		}
	}
	if got, err := Load(path); err != nil || len(got.Entries) != 20 {
		t.Fatalf("index.json unreadable after concurrent saves: %v", err)
	}
}
