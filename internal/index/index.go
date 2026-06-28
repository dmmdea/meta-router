// Package index builds and persists the skill catalog + embedding vectors that
// the per-prompt hook loads. JSON on disk (stdlib-only; ~200 skills × 768
// floats is a couple MB and loads in well under the latency budget).
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/retrievers"
)

// Seams for testing the diff logic without harvesting the real FS or calling
// the embedder. nil → use the real implementations.
var (
	harvestFn func(roots []string) ([]catalog.Skill, error)
	embedFn   func(endpoint string, timeout time.Duration, inputs []string) ([][]float64, error)
)

func harvest(roots []string) ([]catalog.Skill, error) {
	if harvestFn != nil {
		return harvestFn(roots)
	}
	raw, err := catalog.Harvest(roots)
	if err != nil {
		return nil, err
	}
	return catalog.DedupByID(raw), nil
}

func embedTexts(endpoint string, timeout time.Duration, inputs []string) ([][]float64, error) {
	if embedFn != nil {
		return embedFn(endpoint, timeout, inputs)
	}
	return retrievers.EmbedTexts(endpoint, timeout, inputs)
}

type Entry struct {
	Skill catalog.Skill `json:"skill"`
	Vec   []float64     `json:"vec"`
	Hash  string        `json:"hash"`
}

type Index struct {
	Model     string  `json:"model"`
	Dim       int     `json:"dim"`
	BuiltUnix int64   `json:"built_unix"`
	Entries   []Entry `json:"entries"`
}

// HashSkill hashes exactly the text that gets embedded, so a change to any
// embedded field invalidates the cached vector (Task 5 hash-diff).
func HashSkill(s catalog.Skill) string {
	sum := sha256.Sum256([]byte(s.EmbedText()))
	return hex.EncodeToString(sum[:])
}

// Build embeds all skills in one batch and returns a fresh index.
func Build(skills []catalog.Skill, endpoint string, timeout time.Duration) (*Index, error) {
	idx := &Index{Model: "embeddinggemma", BuiltUnix: time.Now().Unix()}
	if len(skills) == 0 {
		return idx, nil // nothing to embed; empty index (Dim 0)
	}
	texts := make([]string, len(skills))
	for i, s := range skills {
		texts[i] = s.EmbedText()
	}
	vecs, err := embedTexts(endpoint, timeout, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(skills) {
		return nil, fmt.Errorf("index: embedder returned %d vecs for %d skills", len(vecs), len(skills))
	}
	idx.Dim = len(vecs[0])
	idx.Entries = make([]Entry, len(skills))
	for i, s := range skills {
		idx.Entries[i] = Entry{Skill: s, Vec: vecs[i], Hash: HashSkill(s)}
	}
	return idx, nil
}

func (idx *Index) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil { // atomic replace
		os.Remove(tmp) // don't leave a stale .tmp if rename fails (e.g. dest locked on Windows)
		return err
	}
	return nil
}

func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func (idx *Index) Skills() []catalog.Skill {
	out := make([]catalog.Skill, len(idx.Entries))
	for i, e := range idx.Entries {
		out[i] = e.Skill
	}
	return out
}

func (idx *Index) Vectors() [][]float64 {
	out := make([][]float64, len(idx.Entries))
	for i, e := range idx.Entries {
		out[i] = e.Vec
	}
	return out
}

// Refresh re-harvests skills and re-embeds only those whose content hash
// changed (or are new); unchanged skills keep their cached vectors, removed
// skills are dropped. Cheap enough to run on every SessionStart.
func (idx *Index) Refresh(roots []string, endpoint string, timeout time.Duration) (added, updated, removed int, err error) {
	cur, err := harvest(roots)
	if err != nil {
		return 0, 0, 0, err
	}
	old := make(map[string]Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		old[e.Skill.ID] = e
	}
	curIDs := make(map[string]bool, len(cur))

	newEntries := make([]Entry, 0, len(cur))
	var toText []string
	var toPos []int
	for _, s := range cur {
		curIDs[s.ID] = true
		h := HashSkill(s)
		if e, ok := old[s.ID]; ok && e.Hash == h {
			newEntries = append(newEntries, Entry{Skill: s, Vec: e.Vec, Hash: h}) // reuse vector, refresh metadata
			continue
		}
		newEntries = append(newEntries, Entry{Skill: s, Hash: h}) // vector filled below
		toText = append(toText, s.EmbedText())
		toPos = append(toPos, len(newEntries)-1)
		if _, ok := old[s.ID]; ok {
			updated++
		} else {
			added++
		}
	}
	for id := range old {
		if !curIDs[id] {
			removed++
		}
	}
	if len(toText) > 0 {
		vecs, e := embedTexts(endpoint, timeout, toText)
		if e != nil {
			return 0, 0, 0, e
		}
		if len(vecs) != len(toText) {
			return 0, 0, 0, fmt.Errorf("index: embedder returned %d vecs for %d inputs", len(vecs), len(toText))
		}
		for j, pos := range toPos {
			newEntries[pos].Vec = vecs[j]
		}
		if idx.Dim == 0 && len(vecs) > 0 && len(vecs[0]) > 0 {
			idx.Dim = len(vecs[0])
		}
	}
	idx.Entries = newEntries
	idx.BuiltUnix = time.Now().Unix()
	return added, updated, removed, nil
}

// DefaultIndexPath is ~/.meta-router/index.json.
func DefaultIndexPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".meta-router", "index.json"), nil
}
