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
	harvestFn func(roots []catalog.Root) ([]catalog.Skill, error)
	embedFn   func(endpoint string, timeout time.Duration, inputs []string) ([][]float64, error)
)

// HarvestSkills runs the canonical harvest + hygiene pipeline over the roots.
// Exported so mr-index can harvest once, apply the removal guard, and then
// refresh from the same snapshot.
func HarvestSkills(roots []catalog.Root) ([]catalog.Skill, error) {
	if harvestFn != nil {
		return harvestFn(roots)
	}
	raw, err := catalog.HarvestRoots(roots)
	if err != nil {
		return nil, err
	}
	return catalog.Dedup(raw), nil
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
	// Write the fast-load sidecar AFTER the JSON so its mtime is >= the
	// JSON's (LoadFast's freshness condition). Best-effort: a failed sidecar
	// write must not fail the save — but never leave a stale one behind,
	// because a stale-but-newer-looking sidecar would win the mtime check.
	if err := idx.saveBin(BinPath(path)); err != nil {
		os.Remove(BinPath(path))
		fmt.Fprintf(os.Stderr, "warning: index sidecar not written (%v); hook will parse JSON\n", err)
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

// RefreshPlan is the pure diff between the current index and a fresh
// harvest: what would be added, re-embedded, and removed. Computing the plan
// before applying it lets callers refuse suspicious mass removals (MR-6)
// without having touched the index.
type RefreshPlan struct {
	Added      int
	Updated    int
	RemovedIDs []string

	entries []Entry  // next entry set; re-embeds have empty Vec
	toText  []string // texts to embed, parallel to toPos
	toPos   []int    // positions in entries to receive the vectors
}

// Reembeds is the number of entries whose vectors must be recomputed
// (new + changed).
func (p *RefreshPlan) Reembeds() int { return len(p.toText) }

// RemovalExceeds reports whether removing `removed` of `before` entries
// crosses maxFrac (e.g. 0.30). An empty index never triggers the guard.
func RemovalExceeds(before, removed int, maxFrac float64) bool {
	if before <= 0 || removed <= 0 {
		return false
	}
	return float64(removed)/float64(before) > maxFrac
}

// PlanRefresh diffs the index against a harvested skill snapshot. Pure: it
// does not mutate the index and calls no embedder.
func (idx *Index) PlanRefresh(cur []catalog.Skill) *RefreshPlan {
	old := make(map[string]Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		old[e.Skill.ID] = e
	}
	curIDs := make(map[string]bool, len(cur))

	p := &RefreshPlan{entries: make([]Entry, 0, len(cur))}
	for _, s := range cur {
		curIDs[s.ID] = true
		h := HashSkill(s)
		if e, ok := old[s.ID]; ok && e.Hash == h {
			p.entries = append(p.entries, Entry{Skill: s, Vec: e.Vec, Hash: h}) // reuse vector, refresh metadata
			continue
		}
		p.entries = append(p.entries, Entry{Skill: s, Hash: h}) // vector filled on apply
		p.toText = append(p.toText, s.EmbedText())
		p.toPos = append(p.toPos, len(p.entries)-1)
		if _, ok := old[s.ID]; ok {
			p.Updated++
		} else {
			p.Added++
		}
	}
	for _, e := range idx.Entries {
		if !curIDs[e.Skill.ID] {
			p.RemovedIDs = append(p.RemovedIDs, e.Skill.ID)
		}
	}
	return p
}

// ApplyRefresh embeds the plan's changed texts and installs the new entry
// set. On embed failure the index is left untouched.
func (idx *Index) ApplyRefresh(p *RefreshPlan, endpoint string, timeout time.Duration) error {
	if len(p.toText) > 0 {
		vecs, err := embedTexts(endpoint, timeout, p.toText)
		if err != nil {
			return err
		}
		if len(vecs) != len(p.toText) {
			return fmt.Errorf("index: embedder returned %d vecs for %d inputs", len(vecs), len(p.toText))
		}
		for j, pos := range p.toPos {
			p.entries[pos].Vec = vecs[j]
		}
		if idx.Dim == 0 && len(vecs) > 0 && len(vecs[0]) > 0 {
			idx.Dim = len(vecs[0])
		}
	}
	idx.Entries = p.entries
	idx.BuiltUnix = time.Now().Unix()
	return nil
}

// Refresh re-harvests skills and re-embeds only those whose content hash
// changed (or are new); unchanged skills keep their cached vectors, removed
// skills are dropped. Cheap enough to run on every SessionStart. Thin
// harvest→plan→apply wrapper; callers needing the removal guard use the
// pieces directly.
func (idx *Index) Refresh(roots []catalog.Root, endpoint string, timeout time.Duration) (added, updated, removed int, err error) {
	cur, err := HarvestSkills(roots)
	if err != nil {
		return 0, 0, 0, err
	}
	p := idx.PlanRefresh(cur)
	if err := idx.ApplyRefresh(p, endpoint, timeout); err != nil {
		return 0, 0, 0, err
	}
	return p.Added, p.Updated, len(p.RemovedIDs), nil
}

// DefaultIndexPath is ~/.meta-router/index.json.
func DefaultIndexPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".meta-router", "index.json"), nil
}
