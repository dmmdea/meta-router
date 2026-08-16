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
	TsUnix int64 `json:"ts_unix"`
	// SessionID and PromptID make analytics joins EXACT. Without them the only
	// key was a timestamp window, which is session-blind: measured on this
	// fleet, 72.4% of active 10-minute buckets have more than one session
	// running (median 2, max 110), so a window join silently credits one
	// session's invocation to another session's surfacing — that inflated a
	// recorded live metric from ~23% to a claimed 39.7%.
	//
	// Claude Code supplies both on the UserPromptSubmit payload (prompt_id
	// since v2.1.196). They are OPAQUE IDENTIFIERS, never prompt content, so
	// the no-prompt-text posture of this log is unchanged. Both are omitempty:
	// a build that does not supply them must leave the field ABSENT rather than
	// write an empty string, which a downstream join would otherwise treat as
	// one real shared session.
	SessionID    string   `json:"session_id,omitempty"`
	PromptID     string   `json:"prompt_id,omitempty"`
	PromptHash   string   `json:"prompt_hash"`
	PromptLen    int      `json:"prompt_len"`
	Surfaced     []string `json:"surfaced"`
	TopCosine    float64  `json:"top_cosine"`
	LatencyMs    int64    `json:"latency_ms"`
	Mode         string   `json:"mode"` // embed | rerank | hybrid | bm25-fallback | gated-empty | embedder-down | too-short | tpl-mismatch | error
	Err          string   `json:"err,omitempty"`
	NudgeOffload bool     `json:"nudge_offload,omitempty"` // an offload-suitability nudge was appended
	QuotaHint    bool     `json:"quota_hint,omitempty"`    // a quota+route hint was appended (§6c RS1)
	// Cands is the ranked candidate list WITH cosine scores. It is present on
	// the embed and rerank paths (including their gated-empty rows) and absent
	// everywhere else.
	//
	// CAVEAT for analysis, added when rerank was wired: on embed rows Cands is
	// the top-k BY COSINE and Cands[0].Cos == TopCosine. On rerank rows the
	// VALUES are still embed cosines (only the ORDER is the cross-encoder's),
	// so the cosine denominator stays uncontaminated — but the list is k of the
	// top-20 SELECTED by the cross-encoder, in its order, so it is neither
	// sorted by cosine nor the same population. Do not read Cands[0].Cos as the
	// event's top cosine without checking Mode.
	// (original R9.2b note follows)
	// — present on embed and embed-gated-empty rows, absent everywhere else
	// (W9 R9.2b). Hybrid is deliberately excluded: its .Score is an RRF fused
	// rank score, not a cosine, and gated-empty rows carry no ranker
	// discriminator to tell them apart downstream. Without it every retrospective curve
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
