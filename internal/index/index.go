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
	"github.com/dmmdea/meta-router/internal/embedtpl"
	"github.com/dmmdea/meta-router/internal/retrievers"
)

// Seams for testing the diff logic without harvesting the real FS or calling
// the embedder. nil → use the real implementations.
var (
	harvestFn func(roots []catalog.Root) ([]catalog.Skill, error)
	embedFn   func(endpoint string, timeout time.Duration, model string, inputs []string) ([][]float64, error)
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

func embedTexts(endpoint string, timeout time.Duration, model string, inputs []string) ([][]float64, error) {
	if embedFn != nil {
		return embedFn(endpoint, timeout, model, inputs)
	}
	return retrievers.EmbedTexts(endpoint, timeout, model, inputs)
}

type Entry struct {
	Skill catalog.Skill `json:"skill"`
	Vec   []float64     `json:"vec"`
	Hash  string        `json:"hash"`
}

type Index struct {
	// Model is the index IDENTITY: the embedding model, plus "/tplN" when the
	// docs were embedded through a template version (embedtpl). The hook
	// resolves this identity to decide how to embed queries — and refuses,
	// fail-open, when it names a template its binary does not know.
	Model     string  `json:"model"`
	Dim       int     `json:"dim"`
	BuiltUnix int64   `json:"built_unix"`
	Entries   []Entry `json:"entries"`
	// TplGuard is the stale-binary tripwire for templated indexes: every
	// template-aware writer stamps the template version here alongside the
	// identity in Model. A PRE-template binary's structs don't have this
	// field, so its refresh — which re-embeds everything RAW (its hashes all
	// miss) while preserving Model verbatim — drops the guard on save. That
	// is the one poisoning this file format can detect: same model, same
	// dim, templated identity, raw vectors. SpecForIndex refuses a templated
	// identity whose guard is missing or mismatched, turning a silently
	// corrupted index into a loud rebuild instruction. Empty (and omitted)
	// on untemplated indexes, so raw JSON stays byte-identical.
	TplGuard string `json:"tpl_guard,omitempty"`
}

// HashSkill hashes exactly the raw canonical text of a skill (the legacy,
// untemplated hash). Kept for compatibility; spec-aware callers use
// HashSkillSpec so a template change invalidates every cached vector.
func HashSkill(s catalog.Skill) string {
	return HashSkillSpec(s, embedtpl.Spec{})
}

// HashSkillSpec hashes exactly the text that gets embedded under spec —
// template INCLUDED — so a change to any embedded field OR to the doc
// template invalidates the cached vector (Task 5 hash-diff; W9-P item 1).
// For an untemplated spec the bytes (and hashes) are identical to HashSkill,
// which is what keeps an in-place upgrade from re-embedding anything.
func HashSkillSpec(s catalog.Skill, spec embedtpl.Spec) string {
	sum := sha256.Sum256([]byte(spec.ApplyDoc(s.EmbedText())))
	return hex.EncodeToString(sum[:])
}

// SpecForIndex resolves the spec that built (and must query) this index.
// Beyond the registry lookup, it enforces the TplGuard tripwire: a templated
// identity whose guard is absent or names a different version means the
// vectors were last written by a binary that doesn't speak templates (or a
// different one) — raw vectors under a templated label, the silent mix.
func (idx *Index) SpecForIndex() (embedtpl.Spec, error) {
	spec, err := embedtpl.SpecForIndex(idx.Model)
	if err != nil {
		return embedtpl.Spec{}, err
	}
	if spec.Version != "" && idx.TplGuard != spec.Version {
		return embedtpl.Spec{}, fmt.Errorf("index claims template %s but its template guard reads %q — the vectors were likely re-embedded raw by a pre-template mr-index (deploy binaries fleet-wide before building templated indexes); rebuild with `mr-index build -tpl %s`", idx.Model, idx.TplGuard, spec.Version)
	}
	return spec, nil
}

// Build embeds all skills in one batch and returns a fresh index whose
// identity records spec (model + template version).
func Build(skills []catalog.Skill, endpoint string, timeout time.Duration, spec embedtpl.Spec) (*Index, error) {
	idx := &Index{Model: spec.Identity(), TplGuard: spec.Version, BuiltUnix: time.Now().Unix()}
	if len(skills) == 0 {
		return idx, nil // nothing to embed; empty index (Dim 0)
	}
	texts := make([]string, len(skills))
	for i, s := range skills {
		texts[i] = spec.ApplyDoc(s.EmbedText())
	}
	vecs, err := embedTexts(endpoint, timeout, spec.Model, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(skills) {
		return nil, fmt.Errorf("index: embedder returned %d vecs for %d skills", len(vecs), len(skills))
	}
	idx.Dim = len(vecs[0])
	idx.Entries = make([]Entry, len(skills))
	for i, s := range skills {
		idx.Entries[i] = Entry{Skill: s, Vec: vecs[i], Hash: HashSkillSpec(s, spec)}
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
	if err := renameAtomic(tmp, path); err != nil { // atomic replace, retried past a transient Windows lock
		os.Remove(tmp) // don't leave a stale .tmp if the replace never lands
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

	entries []Entry       // next entry set; re-embeds have empty Vec
	toText  []string      // texts to embed, parallel to toPos
	toPos   []int         // positions in entries to receive the vectors
	spec    embedtpl.Spec // the spec the texts were templated under; Apply embeds with it
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

// PlanRefresh diffs the index against a harvested skill snapshot under spec
// (the index's OWN spec — see SpecForIndex; a refresh never changes model or
// template, it preserves the identity it loaded). Pure: it does not mutate
// the index and calls no embedder. The plan CARRIES the spec so Apply can
// never embed under a different one than templated the texts.
func (idx *Index) PlanRefresh(cur []catalog.Skill, spec embedtpl.Spec) *RefreshPlan {
	old := make(map[string]Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		old[e.Skill.ID] = e
	}
	curIDs := make(map[string]bool, len(cur))

	p := &RefreshPlan{entries: make([]Entry, 0, len(cur)), spec: spec}
	for _, s := range cur {
		curIDs[s.ID] = true
		h := HashSkillSpec(s, spec)
		if e, ok := old[s.ID]; ok && e.Hash == h {
			p.entries = append(p.entries, Entry{Skill: s, Vec: e.Vec, Hash: h}) // reuse vector, refresh metadata
			continue
		}
		p.entries = append(p.entries, Entry{Skill: s, Hash: h}) // vector filled on apply
		p.toText = append(p.toText, spec.ApplyDoc(s.EmbedText()))
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

// ApplyRefresh embeds the plan's changed texts (already doc-templated by the
// spec the plan carries) with that same spec's model and installs the new
// entry set. On embed failure the index is left untouched.
func (idx *Index) ApplyRefresh(p *RefreshPlan, endpoint string, timeout time.Duration) error {
	// The plan's spec must MATCH the index's recorded identity — refresh
	// never migrates. Today's callers resolve the spec via SpecForIndex so
	// this cannot fire; it exists for the future caller who builds a plan
	// under the wrong spec (the call site lost its explicit spec parameter),
	// where applying would write mixed-space vectors and mis-stamp the guard
	// (review 2026-08-16 round 2: the previous unconditional re-stamp was
	// the one line that could silently STRIP a guard).
	if _, ver := embedtpl.ParseIdentity(idx.Model); p.spec.Version != ver {
		return fmt.Errorf("index: refresh plan built under template version %q but the index identity %q requires %q — plan and index disagree; rebuild the plan from this index's own spec", p.spec.Version, idx.Model, ver)
	}
	if len(p.toText) > 0 {
		vecs, err := embedTexts(endpoint, timeout, p.spec.Model, p.toText)
		if err != nil {
			return err
		}
		if len(vecs) != len(p.toText) {
			return fmt.Errorf("index: embedder returned %d vecs for %d inputs", len(vecs), len(p.toText))
		}
		// A refresh re-embeds only the CHANGED skills and keeps the cached vectors
		// for the rest. If the endpoint that answered serves a different model than
		// the one that built the index, writing these vectors in would produce a
		// mixed-dimension index on disk — a silent corruption that survives the
		// rotate-backup and reports success. Refuse instead; the index stays intact.
		if idx.Dim != 0 && len(vecs) > 0 && len(vecs[0]) != idx.Dim {
			return fmt.Errorf("index: embedder returned dim %d but the index is dim %d — the endpoint serves a different model than the one that built the index; rebuild it (`mr-index build`) or pin the right endpoint", len(vecs[0]), idx.Dim)
		}
		for j, pos := range p.toPos {
			p.entries[pos].Vec = vecs[j]
		}
		if idx.Dim == 0 && len(vecs) > 0 && len(vecs[0]) > 0 {
			idx.Dim = len(vecs[0])
		}
	}
	idx.Entries = p.entries
	// Re-stamp the tripwire: a template-aware writer always leaves its mark,
	// so only a pre-template binary's save can strip it.
	idx.TplGuard = p.spec.Version
	idx.BuiltUnix = time.Now().Unix()
	return nil
}

// Refresh re-harvests skills and re-embeds only those whose content hash
// changed (or are new); unchanged skills keep their cached vectors, removed
// skills are dropped. Cheap enough to run on every SessionStart. Thin
// harvest→plan→apply wrapper; callers needing the removal guard use the
// pieces directly.
func (idx *Index) Refresh(roots []catalog.Root, endpoint string, timeout time.Duration) (added, updated, removed int, err error) {
	// The refresh embeds under the index's OWN recorded identity — never a
	// different model or template. An identity this binary cannot resolve is
	// fatal before anything is harvested or embedded: proceeding would mix
	// vector spaces inside one index.
	spec, err := idx.SpecForIndex()
	if err != nil {
		return 0, 0, 0, err
	}
	cur, err := HarvestSkills(roots)
	if err != nil {
		return 0, 0, 0, err
	}
	p := idx.PlanRefresh(cur, spec)
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
