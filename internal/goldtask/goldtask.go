// Package goldtask is the V1 routing gold set: the 56 locked tasks
// (docs/superpowers/plans/2026-07-15-v1-goldset-selection.md) as a
// machine-readable JSONL, with the schema, loader, and structural validation.
// It is DISTINCT from internal/goldset (skill-retrieval test data): goldtask
// records are routed TASKS with programmatic verifiers — the oracle substrate
// for the V2 all-lanes replay.
//
// Verifier kinds:
//   - "pure"    — regex-key check over the candidate's text output (covers
//     V-FACTS, V-FINDINGS, and sampled V-LABELS; V0-auditable in-suite).
//   - "vgo" / "vpy" / "vpester" — execution-receipt gates run by
//     cmd/mr-goldverify (worktree at Parent, apply candidate diff, drop in the
//     held-out TestFiles from Resolving, run tests). Each carries a PreGate —
//     a cheap pure shape check — so the V0 null/trivial audit covers it too.
package goldtask

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Key is one required (or forbidden) pattern: case-insensitive RE2, matched
// against the candidate's output.
type Key struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// VerifySpec is a task's programmatic verifier.
type VerifySpec struct {
	Kind         string   `json:"kind"`                    // pure | vgo | vpy | vpester
	Keys         []Key    `json:"keys,omitempty"`          // pure: required patterns
	MinKeys      int      `json:"min_keys,omitempty"`      // pure: 0 => every key required
	Forbidden    []Key    `json:"forbidden,omitempty"`     // pure: any match => fail
	PreGate      []Key    `json:"pre_gate,omitempty"`      // exec kinds: all-required static gate (V0 face)
	Repo         string   `json:"repo,omitempty"`          // exec: logical repo name
	Parent       string   `json:"parent,omitempty"`        // exec: checkout point given to the agent
	Resolving    string   `json:"resolving,omitempty"`     // exec: ground-truth commit (held-out tests live here)
	TestFiles    []string `json:"test_files,omitempty"`    // exec: files copied from Resolving into the worktree
	Pkgs         []string `json:"pkgs,omitempty"`          // vgo: go test targets
	TestCmd      string   `json:"test_cmd,omitempty"`      // vpy / vpester command
	AllowedFiles []string `json:"allowed_files,omitempty"` // optional: candidate diff must stay inside
}

// Task is one gold-set record.
type Task struct {
	ID     string     `json:"id"`
	Class  string     `json:"class"` // agentic-coding | quick-edit | research | extraction | review
	Split  string     `json:"split"` // tuning | heldout
	Repo   string     `json:"repo"`  // logical source repo
	Prompt string     `json:"prompt"`
	Verify VerifySpec `json:"verify"`
}

var validClasses = map[string]bool{
	"agentic-coding": true, "quick-edit": true, "research": true,
	"extraction": true, "review": true,
}

var validKinds = map[string]bool{"pure": true, "vgo": true, "vpy": true, "vpester": true}

// Load reads the gold set (one Task per line), mirroring goldset.Load
// conventions: a missing file errors; a torn line is skipped, not fatal.
// Structural correctness is Validate's job, not Load's.
func Load(path string) ([]Task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Task
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var t Task
		if json.Unmarshal(line, &t) != nil {
			continue
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

// Validate enforces the record contract: unique IDs, known class/split/kind,
// non-empty prompt, and per-kind required fields. It does NOT enforce the
// 56-task selection shape — that lives in the structural test, which is the
// executable form of the locked selection doc.
func Validate(ts []Task) error {
	seen := map[string]bool{}
	for i, t := range ts {
		where := fmt.Sprintf("record %d (%s)", i, t.ID)
		if t.ID == "" || seen[t.ID] {
			return fmt.Errorf("%s: empty or duplicate id", where)
		}
		seen[t.ID] = true
		if !validClasses[t.Class] {
			return fmt.Errorf("%s: invalid class %q", where, t.Class)
		}
		if t.Split != "tuning" && t.Split != "heldout" {
			return fmt.Errorf("%s: invalid split %q", where, t.Split)
		}
		if t.Repo == "" {
			return fmt.Errorf("%s: empty repo", where)
		}
		if t.Prompt == "" {
			return fmt.Errorf("%s: empty prompt", where)
		}
		v := t.Verify
		if !validKinds[v.Kind] {
			return fmt.Errorf("%s: invalid verify kind %q", where, v.Kind)
		}
		if v.Kind == "pure" {
			if len(v.Keys) == 0 {
				return fmt.Errorf("%s: pure verifier with no keys", where)
			}
		} else {
			if v.Repo == "" || v.Parent == "" || v.Resolving == "" {
				return fmt.Errorf("%s: exec verifier missing repo/parent/resolving", where)
			}
			if len(v.PreGate) == 0 {
				return fmt.Errorf("%s: exec verifier missing pre_gate (V0 audit face)", where)
			}
			if v.Kind == "vgo" && len(v.Pkgs) == 0 {
				return fmt.Errorf("%s: vgo verifier missing pkgs", where)
			}
			if (v.Kind == "vpy" || v.Kind == "vpester") && v.TestCmd == "" {
				return fmt.Errorf("%s: %s verifier missing test_cmd", where, v.Kind)
			}
		}
	}
	return nil
}
