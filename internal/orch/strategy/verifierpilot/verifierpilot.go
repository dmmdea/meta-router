// Package verifierpilot is the S3R-10c seed-dataset shape for the
// verifier-ceiling pilot: raw per-snippet agree/disagree records that slice-4's
// gold-set verifier harness can consume directly (a seed dataset, not a
// throwaway number). The pilot runs the LOCAL lane (offload-harness triage door,
// a black box) as a tier-2 critic over code snippets with KNOWN good/bad labels
// and records whether the critic's verdict agreed with the label.
//
// The shape mirrors internal/goldset's JSONL convention (one JSON object per
// line) so slice-4 can load it with the same pattern. It is a PILOT: n is small
// by design and labeled as such — never presented as a benchmark.
package verifierpilot

import (
	"bufio"
	"encoding/json"
	"os"
)

// Verdict is the local critic's yes/no/defer decision, normalized from the
// offload-harness triage door's {decision} field (a DEFER maps to VerdictDefer —
// an honest relegation, never a false pass/fail).
type Verdict string

const (
	VerdictPass    Verdict = "pass"  // critic judged the snippet good ("yes")
	VerdictFail    Verdict = "fail"  // critic judged the snippet bad ("no")
	VerdictDefer   Verdict = "defer" // critic honestly relegated (structured DEFER)
	VerdictErrored Verdict = "error" // spawn_error / parse_error — the fail-open path
)

// Label is the ground-truth good/bad label for a snippet.
type Label string

const (
	LabelGood Label = "good"
	LabelBad  Label = "bad"
)

// Record is one pilot data point: a labeled snippet + the local critic's verdict
// + whether they agreed. This is the exact shape slice-4's gold-set harness
// consumes (JSONL, one Record per line).
type Record struct {
	Snippet    string  `json:"snippet"`              // the code (or a stable id/path in a larger set)
	Label      Label   `json:"label"`                // ground truth: good | bad
	Verdict    Verdict `json:"verdict"`              // local critic decision: pass | fail | defer | error
	Agree      bool    `json:"agree"`                // verdict matched the label (defer/error never agree)
	Confidence float64 `json:"confidence,omitempty"` // graded logprob decision margin; 0 = non-answer (defer/error)
	Reason     string  `json:"reason,omitempty"`     // the critic's stated reason (black-box, advisory)
	Model      string  `json:"model,omitempty"`      // the resolved local model tier (e.g. gemma4-e2b)
	LatencyMS  int64   `json:"latency_ms,omitempty"`
}

// Agreement computes whether a verdict agrees with a label. Only a decisive
// pass/fail that matches the label agrees; a defer or an error NEVER counts as
// agreement (an honest relegation is neither right nor wrong on the label — it
// is a non-answer, exactly the verifier-ceiling signal slice-4 needs to weigh).
func Agreement(label Label, v Verdict) bool {
	switch v {
	case VerdictPass:
		return label == LabelGood
	case VerdictFail:
		return label == LabelBad
	default:
		return false
	}
}

// Summary is the roll-up of a pilot run: raw counts, honestly labeled as a pilot.
type Summary struct {
	N        int  `json:"n"`
	Agree    int  `json:"agree"`
	Disagree int  `json:"disagree"`
	Deferred int  `json:"deferred"`
	Errored  int  `json:"errored"`
	FailOpen bool `json:"fail_open"` // true when the binaries were not on PATH — pilot deferred, fail-open verified
}

// Summarize rolls up records into raw counts. Disagree counts only decisive
// verdicts that were wrong (a defer/error is tallied separately, never as a
// disagreement) so the agree/disagree ratio reflects the critic's decisive
// accuracy and the defer rate is visible on its own.
func Summarize(recs []Record) Summary {
	var s Summary
	s.N = len(recs)
	for _, r := range recs {
		switch r.Verdict {
		case VerdictDefer:
			s.Deferred++
		case VerdictErrored:
			s.Errored++
		default:
			if r.Agree {
				s.Agree++
			} else {
				s.Disagree++
			}
		}
	}
	return s
}

// Load reads a JSONL pilot dataset (one Record per line), mirroring
// goldset.Load: a missing file errors; a torn line is skipped, not fatal.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Marshal renders records as JSONL (one Record per line) for writing the seed
// dataset. Deterministic field order via encoding/json.
func Marshal(recs []Record) ([]byte, error) {
	var out []byte
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out, nil
}

// Snippet is one labeled INPUT to the ceiling harness: code (or a stable id) +
// its ground-truth good/bad label. Distinct from Record, which adds the critic's
// verdict/confidence after a run.
type Snippet struct {
	ID      string `json:"id,omitempty"`
	Snippet string `json:"snippet"`
	Label   Label  `json:"label"`
}

// LoadSnippets reads a labeled snippet corpus (one Snippet per line), mirroring
// Load: a missing file errors; a torn line is skipped, not fatal.
func LoadSnippets(path string) ([]Snippet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Snippet
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s Snippet
		if json.Unmarshal(line, &s) != nil {
			continue
		}
		out = append(out, s)
	}
	return out, sc.Err()
}
