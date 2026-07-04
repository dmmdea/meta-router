package index

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
)

func syntheticIndex(n, dim int) *Index {
	rng := rand.New(rand.NewSource(42))
	idx := &Index{Model: "embeddinggemma", Dim: dim, BuiltUnix: 1234}
	for i := 0; i < n; i++ {
		s := catalog.Skill{
			ID:          "skill-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Name:        "skill",
			Description: "a synthetic skill entry with a plausible description length for benchmarking the loader",
			Source:      "skills",
		}
		v := make([]float64, dim)
		for j := range v {
			v[j] = rng.Float64()*2 - 1
		}
		idx.Entries = append(idx.Entries, Entry{Skill: s, Vec: v, Hash: HashSkill(s)})
	}
	return idx
}

func TestSaveWritesSidecarAndLoadFastUsesIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	idx := syntheticIndex(5, 8)
	if err := idx.Save(path); err != nil {
		t.Fatal(err)
	}
	binP := BinPath(path)
	if _, err := os.Stat(binP); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	// Remove the JSON: LoadFast must still succeed purely from the sidecar,
	// proving it does not merely fall back to JSON.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFast(path)
	if err != nil {
		t.Fatalf("LoadFast from sidecar alone: %v", err)
	}
	if len(got.Entries) != 5 || got.Dim != 8 || got.Model != "embeddinggemma" {
		t.Fatalf("sidecar roundtrip mismatch: %+v", got)
	}
	// float32 roundtrip: within float32 epsilon of the original.
	for i, e := range got.Entries {
		for j, x := range e.Vec {
			if math.Abs(x-idx.Entries[i].Vec[j]) > 1e-6 {
				t.Fatalf("vector fidelity lost at [%d][%d]: %v vs %v", i, j, x, idx.Entries[i].Vec[j])
			}
		}
	}
}

func TestLoadFast_StaleSidecarFallsBackToJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := syntheticIndex(3, 4).Save(path); err != nil {
		t.Fatal(err)
	}
	// Simulate a JSON-only writer (e.g. an older tool) updating the index
	// after the sidecar was produced: overwrite the JSON with a 7-entry
	// index WITHOUT touching the sidecar, and age the sidecar's mtime.
	data, err := json.Marshal(syntheticIndex(7, 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(BinPath(path), old, old); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 7 {
		t.Fatalf("stale sidecar must lose to the fresher JSON: got %d entries, want 7", len(got.Entries))
	}
}

func TestLoadFast_CorruptFreshSidecarFallsBackToJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	idx := syntheticIndex(4, 4)
	if err := idx.Save(path); err != nil {
		t.Fatal(err)
	}
	// Corrupt the (fresh) sidecar: decode fails → JSON fallback.
	if err := os.WriteFile(BinPath(path), []byte("not a gob stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 4 {
		t.Fatalf("corrupt sidecar fallback failed: %d entries", len(got.Entries))
	}
}

// Benchmarks: the per-prompt hook parses the index every invocation, so load
// time is directly user-visible latency. Run with:
//
//	go test ./internal/index/ -bench BenchmarkLoad -benchmem
func benchIndexPair(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "index.json")
	// ~150 skills × 768 dims ≈ the real production index (2.5MB JSON).
	if err := syntheticIndex(150, 768).Save(path); err != nil {
		b.Fatal(err)
	}
	return path
}

func BenchmarkLoadJSON(b *testing.B) {
	path := benchIndexPair(b)
	os.Remove(BinPath(path)) // force the JSON path
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Load(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoadBin(b *testing.B) {
	path := benchIndexPair(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := LoadFast(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoadReal measures against a real index copied outside the repo:
//
//	MR_REAL_INDEX=path\to\index.json go test ./internal/index/ -bench LoadReal
func BenchmarkLoadReal(b *testing.B) {
	path := os.Getenv("MR_REAL_INDEX")
	if path == "" {
		b.Skip("MR_REAL_INDEX not set")
	}
	idx, err := Load(path)
	if err != nil {
		b.Fatal(err)
	}
	work := filepath.Join(b.TempDir(), "index.json")
	if err := idx.Save(work); err != nil {
		b.Fatal(err)
	}
	b.Run("json", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Load(work); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bin", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := LoadFast(work); err != nil {
				b.Fatal(err)
			}
		}
	})
}
