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
	if len(deps) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, d := range deps {
		ss := st[d]
		if ss == nil || ss.ResultRef == "" {
			fmt.Fprintf(&b, "<context from=step-%d missing />\n", d)
			continue
		}
		a, err := ReadArtifact(ss.ResultRef)
		if err != nil {
			return "", fmt.Errorf("resolve context for dep %d: %w", d, err)
		}
		fmt.Fprintf(&b, "<context from=step-%d>\n%s\n</context>\n", d, a.Content)
	}
	return b.String(), nil
}
