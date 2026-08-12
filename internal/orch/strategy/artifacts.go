package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dmmdea/meta-router/internal/orch/compact"
)

type Artifact struct {
	StepID       int    `json:"step_id"`
	OutcomeClass string `json:"outcome_class"`
	Content      string `json:"content"`
	SHA256       string `json:"sha256"`
}

// WriteArtifact writes artifacts/<step_id>.json and returns its path as the
// result_ref. The sha256 is over the content (dedupe/audit; content-addressed
// per Bernstein — advisory, §7). Write is temp+atomic-rename so a crash never
// leaves a torn artifact a downstream node could read as truncated context.
func WriteArtifact(dir string, a Artifact) (string, error) {
	adir := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(a.Content))
	a.SHA256 = hex.EncodeToString(sum[:])
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", err
	}
	ref := filepath.Join(adir, strconv.Itoa(a.StepID)+".json")
	tmp := fmt.Sprintf("%s.tmp.%d", ref, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, ref); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return ref, nil
}

func ReadArtifact(ref string) (Artifact, error) {
	b, err := os.ReadFile(ref)
	if err != nil {
		return Artifact{}, err
	}
	var a Artifact
	err = json.Unmarshal(b, &a)
	return a, err
}

// ResolveContext assembles the dep-context prompt block for a node from ONLY the
// artifacts its deps name (Fugu-Ultra context isolation: a step sees ONLY its
// deps' stored artifacts, never a non-dep artifact, never a raw transcript).
// Each dep's content is fenced with its step id so the downstream node can tell
// inputs apart. A dep with no recorded ResultRef is skipped with a marker (never
// a hard error — the executor already gated readiness on outcome_class=="ok").
// A root node (no deps) gets an empty block, so its prompt is the bare
// instruction with no injected context.
func ResolveContext(dir string, deps []int, st map[int]*StepState) (string, error) {
	s, _, err := resolveContext(dir, deps, st, false)
	return s, err
}

// ResolveContextCompact is ResolveContext with W5's LOSSLESS embed-time
// compaction: a dep whose artifact content is JSON is embedded in its
// compacted form (minified + columnar rotation — provable round-trip, see
// internal/orch/compact). The STORED artifact is never rewritten, so the
// original bytes stay recoverable regardless; savings are the returned
// byte count (the R14 side-effect metric — reported, never the goal).
// Prose content embeds byte-identical: only JSON has a lossless compact form.
func ResolveContextCompact(dir string, deps []int, st map[int]*StepState) (string, int, error) {
	return resolveContext(dir, deps, st, true)
}

func resolveContext(dir string, deps []int, st map[int]*StepState, compactOn bool) (string, int, error) {
	if len(deps) == 0 {
		return "", 0, nil
	}
	saved := 0
	var b strings.Builder
	for _, d := range deps {
		ss := st[d]
		if ss == nil || ss.ResultRef == "" {
			fmt.Fprintf(&b, "<context from=step-%d missing />\n", d)
			continue
		}
		a, err := ReadArtifact(ss.ResultRef)
		if err != nil {
			return "", 0, fmt.Errorf("resolve context for dep %d: %w", d, err)
		}
		content := a.Content
		if compactOn {
			// EmbedSafe, not Compact: the consumer is a MODEL that never
			// decompacts, so a marker-family document embeds untouched.
			if c, ok := compact.EmbedSafe(content); ok && len(c) < len(content) {
				saved += len(content) - len(c)
				content = c
			}
		}
		fmt.Fprintf(&b, "<context from=step-%d>\n%s\n</context>\n", d, content)
	}
	return b.String(), saved, nil
}
