package index

import (
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// MR-11: the hook parses the index on EVERY prompt. The 2.5MB JSON form
// spends most of its load decoding ~150×768 float64 literals; a gob sidecar
// with float32 vectors decodes ~an order of magnitude faster and is written
// alongside the JSON on every save. JSON stays the source of truth (human
// inspectable, schema-stable); the sidecar is a pure cache — LoadFast only
// trusts it when it is at least as fresh as the JSON, and any decode problem
// falls back to JSON. float32 keeps ~7 significant digits, far beyond what
// cosine ranking of unit-normalized embeddings can distinguish.

// binFormatVersion is bumped on incompatible sidecar changes; a mismatched
// version is treated as a decode failure (fall back to JSON).
const binFormatVersion = 1

type binEntry struct {
	Skill catalog.Skill
	Vec   []float32
	Hash  string
}

type binIndex struct {
	Version   int
	Model     string
	Dim       int
	BuiltUnix int64
	Entries   []binEntry
}

// BinPath maps an index path to its sidecar path (index.json → index.bin).
func BinPath(path string) string {
	if strings.HasSuffix(path, ".json") {
		return strings.TrimSuffix(path, ".json") + ".bin"
	}
	return path + ".bin"
}

// saveBin writes the sidecar atomically (tmp + rename).
func (idx *Index) saveBin(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	bi := binIndex{Version: binFormatVersion, Model: idx.Model, Dim: idx.Dim, BuiltUnix: idx.BuiltUnix}
	bi.Entries = make([]binEntry, len(idx.Entries))
	for i, e := range idx.Entries {
		v := make([]float32, len(e.Vec))
		for j, x := range e.Vec {
			v[j] = float32(x)
		}
		bi.Entries[i] = binEntry{Skill: e.Skill, Vec: v, Hash: e.Hash}
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(&bi); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := renameAtomic(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// loadBin decodes the sidecar back into an Index (float32 → float64).
func loadBin(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var bi binIndex
	if err := gob.NewDecoder(f).Decode(&bi); err != nil {
		return nil, err
	}
	if bi.Version != binFormatVersion {
		return nil, fmt.Errorf("index.bin: format version %d (want %d)", bi.Version, binFormatVersion)
	}
	idx := &Index{Model: bi.Model, Dim: bi.Dim, BuiltUnix: bi.BuiltUnix, Entries: make([]Entry, len(bi.Entries))}
	for i, e := range bi.Entries {
		v := make([]float64, len(e.Vec))
		for j, x := range e.Vec {
			v[j] = float64(x)
		}
		idx.Entries[i] = Entry{Skill: e.Skill, Vec: v, Hash: e.Hash}
	}
	return idx, nil
}

// LoadFast loads the sidecar when present and at least as fresh as the JSON
// (mtime), else the JSON. Every sidecar problem — missing, stale, truncated,
// version-mismatched — silently resolves to the JSON path, so the sidecar
// can never make the hook worse than before it existed.
func LoadFast(path string) (*Index, error) {
	binP := BinPath(path)
	bStat, bErr := os.Stat(binP)
	jStat, jErr := os.Stat(path)
	if bErr == nil && (jErr != nil || !bStat.ModTime().Before(jStat.ModTime())) {
		if idx, err := loadBin(binP); err == nil {
			return idx, nil
		}
	}
	return Load(path)
}
