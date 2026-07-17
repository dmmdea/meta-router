// Package calib fits window capacities from quota-trace calibration samples —
// fact-refresh gap #1 closed: SetCapacity finally gets its production caller
// (cmd/mr-orchestrate status → maybeFit). Every traced provider observation
// pairs this device's shadow count with the account's used percentage; each
// eligible pair implies capacity ≈ shadow×100/pct, and a median-agreement fit
// across enough agreeing rows yields a learned cap.
//
// Known bias, recorded: shadow undercounts (other devices / untee'd usage) ⇒
// the fit UNDERestimates capacity ⇒ derived percentages read HIGH ⇒ admission
// errs conservative. Correct direction for a safety substrate.
package calib

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"slices"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// Sample is one quota-trace.jsonl row (the quotasig traceRow shape; unknown
// fields like resets_at are ignored on unmarshal).
type Sample struct {
	TS           time.Time `json:"ts"`
	Lane, Window string
	UsedPct      float64 `json:"used_pct"`
	ShadowTokens int64   `json:"shadow_tokens"`
}

// Options tunes the fit. Zero values backfill from Defaults (hand-edit-damage
// rule, consistent with orchcfg tier numerics).
type Options struct {
	MinSamples int     // fewer agreeing estimates than this ⇒ no fit
	MinPct     float64 // rows below this depletion are noise (tiny pct ⇒ huge relative error)
	TolPct     float64 // agreement band around the median, in percent
}

func Defaults() Options { return Options{MinSamples: 5, MinPct: 20, TolPct: 15} }

// Load reads the JSONL trace fail-open: a missing file is nil; an unparseable
// line is skipped (a torn append must not void the readable history).
func Load(path string) []Sample {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []Sample
	for _, ln := range bytes.Split(raw, []byte("\n")) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		var s Sample
		if json.Unmarshal(ln, &s) == nil && s.Lane != "" && s.Window != "" {
			out = append(out, s)
		}
	}
	return out
}

// Fit estimates a (lane, window) capacity by median agreement. Per eligible
// row (UsedPct >= MinPct && ShadowTokens > 0): estimate = ShadowTokens×100/
// UsedPct. ok ⇔ at least MinSamples estimates lie within ±TolPct of their
// median; the result is the median of that agreeing set and n its size.
// No fit is a normal outcome (sparse/noisy trace), not an error.
func Fit(samples []Sample, lane string, w ledger.WindowKind, opt Options) (capTokens int64, n int, ok bool) {
	d := Defaults()
	if opt.MinSamples <= 0 {
		opt.MinSamples = d.MinSamples
	}
	if opt.MinPct <= 0 {
		opt.MinPct = d.MinPct
	}
	if opt.TolPct <= 0 {
		opt.TolPct = d.TolPct
	}
	var est []float64
	for _, s := range samples {
		if s.Lane != lane || s.Window != string(w) {
			continue
		}
		if s.UsedPct < opt.MinPct || s.ShadowTokens <= 0 {
			continue
		}
		est = append(est, float64(s.ShadowTokens)*100/s.UsedPct)
	}
	if len(est) < opt.MinSamples {
		return 0, len(est), false
	}
	med := median(est)
	var agree []float64
	for _, e := range est {
		if math.Abs(e-med) <= med*opt.TolPct/100 {
			agree = append(agree, e)
		}
	}
	if len(agree) < opt.MinSamples {
		return 0, len(agree), false
	}
	return int64(math.Round(median(agree))), len(agree), true
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}
