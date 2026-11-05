package index

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	// TplGuard mirrors Index.TplGuard (the stale-binary tripwire). Gob
	// tolerates the field's absence in old sidecars — those were written by
	// raw-identity builds, where an empty guard is correct.
	TplGuard string
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
	bi := binIndex{Version: binFormatVersion, Model: idx.Model, Dim: idx.Dim, BuiltUnix: idx.BuiltUnix, TplGuard: idx.TplGuard}
	bi.Entries = make([]binEntry, len(idx.Entries))
	for i, e := range idx.Entries {
		v := make([]float32, len(e.Vec))
		for j, x := range e.Vec {
			v[j] = float32(x)
		}
		bi.Entries[i] = binEntry{Skill: e.Skill, Vec: v, Hash: e.Hash}
	}
	return writeAtomic(path, func(w io.Writer) error { return gob.NewEncoder(w).Encode(&bi) })
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
	idx := &Index{Model: bi.Model, Dim: bi.Dim, BuiltUnix: bi.BuiltUnix, TplGuard: bi.TplGuard, Entries: make([]Entry, len(bi.Entries))}
	for i, e := range bi.Entries {
		v := make([]float64, len(e.Vec))
		for j, x := range e.Vec {
			v[j] = float64(x)
		}
		idx.Entries[i] = Entry{Skill: e.Skill, Vec: v, Hash: e.Hash}
	}
	return idx, nil
}

// LoadFast loads the index the way the hook wants it: the sidecar when it is
// at least as fresh as the JSON, else the JSON, and — if the JSON is torn —
// whatever LoadWithFallback can recover. See LoadWithFallback.
func LoadFast(path string) (*Index, error) {
	idx, _, err := LoadWithFallback(path)
	return idx, err
}

// LoadWithFallback loads the index and says where it came from (source):
//   - "bin": the sidecar, present and at least as fresh as the JSON (mtime);
//   - "json": the JSON, when the sidecar is missing, stale or unreadable;
//   - "bin-stale": the JSON does not parse, so the sidecar is used regardless of age;
//   - "bak": the JSON and the sidecar are both unusable; the newest
//     <path>.bak-* that parses.
//
// If nothing parses the JSON's own error is returned. A missing JSON is
// returned as such (callers treat it as "no index yet"). A torn index.json
// once failed every hook run for about 40 hours, because the sidecar was only
// ever consulted when it was fresh.
func LoadWithFallback(path string) (idx *Index, source string, err error) {
	binP := BinPath(path)
	bStat, bErr := os.Stat(binP)
	jStat, jErr := os.Stat(path)
	if bErr == nil && (jErr != nil || !bStat.ModTime().Before(jStat.ModTime())) {
		if idx, err := loadBin(binP); err == nil {
			return idx, "bin", nil
		}
	}
	idx, err = Load(path)
	if err == nil {
		return idx, "json", nil
	}
	if jErr != nil && os.IsNotExist(err) {
		return nil, "", err
	}
	if bErr == nil {
		if b, berr := loadBin(binP); berr == nil {
			return b, "bin-stale", nil
		}
	}
	baks, _ := filepath.Glob(path + ".bak-*")
	sort.Slice(baks, func(i, j int) bool { return modTime(baks[i]).After(modTime(baks[j])) })
	for _, p := range baks {
		if b, berr := Load(p); berr == nil {
			return b, "bak", nil
		}
	}
	return nil, "", err
}

func modTime(p string) time.Time {
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
