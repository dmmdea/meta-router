// Package usagelog appends one JSON record per surfacing event. Prompt text is
// never stored raw — only a hash + length (privacy; it's a personal tool).
package usagelog

import (
	"math"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

type Record struct {
	TsUnix       int64    `json:"ts_unix"`
	PromptHash   string   `json:"prompt_hash"`
	PromptLen    int      `json:"prompt_len"`
	Surfaced     []string `json:"surfaced"`
	TopCosine    float64  `json:"top_cosine"`
	LatencyMs    int64    `json:"latency_ms"`
	Mode         string   `json:"mode"` // embed | hybrid | bm25-fallback | gated-empty | embedder-down | too-short | error
	Err          string   `json:"err,omitempty"`
	NudgeOffload bool     `json:"nudge_offload,omitempty"` // an offload-suitability nudge was appended
	QuotaHint    bool     `json:"quota_hint,omitempty"`    // a quota+route hint was appended (§6c RS1)
	// Cands is the ranked candidate list WITH scores from the primary cosine
	// path — present on embed/hybrid AND gated-empty rows, absent on modes
	// where no cosine ran (W9 R9.2b). Without it every retrospective curve
	// had to use the top-1 cosine as a proxy for the invoked skill's own
	// score, making recall-at-gate an upper bound (R9.2 doc, caveat 6).
	// Names + numbers only: the privacy posture (no prompt text) is unchanged.
	Cands []Cand `json:"cands,omitempty"`
}

// Cand is one scored candidate. Cos is rounded to 4 decimals AT MARSHALING —
// not in a constructor a future call site could bypass — because full float64
// precision would bloat every row for no analytical gain, and "rows are
// compact" must hold no matter who builds the value.
type Cand struct {
	ID  string  `json:"id"`
	Cos float64 `json:"cos"`
}

func (c Cand) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID  string  `json:"id"`
		Cos float64 `json:"cos"`
	}
	return json.Marshal(wire{ID: c.ID, Cos: math.Round(c.Cos*10000) / 10000})
}

func HashPrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func Append(path string, r Record) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Surface a failed flush on the durable log (a Close error can mean the
	// write was not persisted) — but don't clobber an earlier error.
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func DefaultLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".meta-router", "usage.jsonl"), nil
}
