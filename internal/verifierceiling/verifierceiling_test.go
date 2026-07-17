package verifierceiling

import (
	"math"
	"testing"

	vp "github.com/dmmdea/meta-router/internal/orch/strategy/verifierpilot"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Worked example (distinct confidences, one wrong, one defer):
//
//	r1 pass/good conf .9 (correct) | r2 fail/good conf .6 (wrong) |
//	r3 pass/good conf .3 (correct) | r4 defer conf 0 (non-answer)
//
// cond risks over k: 0, 1/2, 1/3, 2/4  → AURC = 1.33333/4 = 0.3333333
// gen  risks over k: 0, 1/4, 1/4, 2/4  → AUGRC = 1.0/4     = 0.25
func TestComputeWorkedExample(t *testing.T) {
	recs := []vp.Record{
		{Label: vp.LabelGood, Verdict: vp.VerdictPass, Agree: true, Confidence: 0.9},
		{Label: vp.LabelGood, Verdict: vp.VerdictFail, Agree: false, Confidence: 0.6},
		{Label: vp.LabelGood, Verdict: vp.VerdictPass, Agree: true, Confidence: 0.3},
		{Label: vp.LabelBad, Verdict: vp.VerdictDefer, Agree: false, Confidence: 0},
	}
	c := Compute(recs)
	if c.N != 4 || c.Decisive != 3 || c.Agree != 2 || c.Disagree != 1 || c.Deferred != 1 {
		t.Fatalf("counts wrong: %+v", c)
	}
	if !almost(c.DecisiveAccuracy, 2.0/3.0) || !almost(c.Coverage, 0.75) || !almost(c.EffectiveCeiling, 0.5) {
		t.Fatalf("rates wrong: acc=%v cov=%v eff=%v", c.DecisiveAccuracy, c.Coverage, c.EffectiveCeiling)
	}
	if !almost(c.AURC, 1.0/3.0) {
		t.Fatalf("AURC=%v want 0.33333", c.AURC)
	}
	if !almost(c.AUGRC, 0.25) {
		t.Fatalf("AUGRC=%v want 0.25", c.AUGRC)
	}
}

// Ties are order-independent: two records at conf .5, one wrong.
// cond over k: 0.5, 0.5 → AURC 0.5 ; gen over k: 0.25, 0.5 → AUGRC 0.375
func TestComputeTiesOrderIndependent(t *testing.T) {
	a := []vp.Record{
		{Label: vp.LabelGood, Verdict: vp.VerdictPass, Agree: true, Confidence: 0.5},
		{Label: vp.LabelGood, Verdict: vp.VerdictFail, Agree: false, Confidence: 0.5},
	}
	b := []vp.Record{a[1], a[0]} // reversed
	ca, cb := Compute(a), Compute(b)
	if !almost(ca.AURC, 0.5) || !almost(ca.AUGRC, 0.375) {
		t.Fatalf("tie AURC/AUGRC wrong: %v %v", ca.AURC, ca.AUGRC)
	}
	if !almost(ca.AURC, cb.AURC) || !almost(ca.AUGRC, cb.AUGRC) {
		t.Fatalf("tie handling order-dependent: %+v vs %+v", ca, cb)
	}
}

// Endpoint invariant: generalized risk at full coverage == 1 - EffectiveCeiling.
func TestGenRiskEndpointInvariant(t *testing.T) {
	recs := []vp.Record{
		{Label: vp.LabelGood, Verdict: vp.VerdictPass, Agree: true, Confidence: 0.8},
		{Label: vp.LabelBad, Verdict: vp.VerdictPass, Agree: false, Confidence: 0.4},
		{Label: vp.LabelGood, Verdict: vp.VerdictDefer, Agree: false, Confidence: 0},
		{Label: vp.LabelBad, Verdict: vp.VerdictErrored, Agree: false, Confidence: 0},
	}
	c := Compute(recs)
	// Agree=1, N=4 → EffectiveCeiling 0.25; wrong+defer+err = 3 → genRisk@full = 0.75.
	if !almost(c.EffectiveCeiling, 0.25) {
		t.Fatalf("eff ceiling=%v", c.EffectiveCeiling)
	}
}

// Degeneracy guard fires on a binary/low-resolution confidence distribution.
func TestDegenerateBinaryConfidence(t *testing.T) {
	var recs []vp.Record
	for i := 0; i < 10; i++ {
		recs = append(recs, vp.Record{Label: vp.LabelGood, Verdict: vp.VerdictPass, Agree: true, Confidence: 1.0})
	}
	c := Compute(recs)
	if !c.Degenerate || c.DegenerateReason == "" {
		t.Fatalf("expected degeneracy flag, got %+v", c)
	}
}

// All-defer verifier: no decisive answers → degenerate, effective ceiling 0.
func TestAllDefer(t *testing.T) {
	recs := []vp.Record{
		{Label: vp.LabelGood, Verdict: vp.VerdictDefer, Confidence: 0},
		{Label: vp.LabelBad, Verdict: vp.VerdictDefer, Confidence: 0},
	}
	c := Compute(recs)
	if c.Decisive != 0 || !almost(c.EffectiveCeiling, 0) || !c.Degenerate {
		t.Fatalf("all-defer wrong: %+v", c)
	}
}
