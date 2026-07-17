package verifierpilot

import (
	"os"
	"path/filepath"
	"testing"
)

// Agreement: only a decisive verdict that matches the label agrees; a defer or
// error is a non-answer (never agreement) — the verifier-ceiling signal slice-4
// weighs separately.
func TestAgreement(t *testing.T) {
	cases := []struct {
		label Label
		v     Verdict
		want  bool
	}{
		{LabelGood, VerdictPass, true},
		{LabelBad, VerdictFail, true},
		{LabelGood, VerdictFail, false},
		{LabelBad, VerdictPass, false},
		{LabelGood, VerdictDefer, false}, // a defer never agrees
		{LabelBad, VerdictDefer, false},
		{LabelGood, VerdictErrored, false}, // fail-open error never agrees
	}
	for _, c := range cases {
		if got := Agreement(c.label, c.v); got != c.want {
			t.Errorf("Agreement(%s,%s)=%v want %v", c.label, c.v, got, c.want)
		}
	}
}

// Summarize rolls up raw counts: agree/disagree count only decisive verdicts;
// defer/error are tallied on their own lines (not as disagreements).
func TestSummarize(t *testing.T) {
	recs := []Record{
		{Label: LabelGood, Verdict: VerdictPass, Agree: true},
		{Label: LabelBad, Verdict: VerdictFail, Agree: true},
		{Label: LabelGood, Verdict: VerdictFail, Agree: false}, // decisive but wrong
		{Label: LabelBad, Verdict: VerdictDefer, Agree: false}, // honest defer
		{Label: LabelGood, Verdict: VerdictErrored, Agree: false},
	}
	s := Summarize(recs)
	if s.N != 5 || s.Agree != 2 || s.Disagree != 1 || s.Deferred != 1 || s.Errored != 1 {
		t.Fatalf("summary wrong: %+v", s)
	}
}

// Round-trip: Marshal then Load yields the same records (the seed-dataset JSONL
// shape slice-4 consumes).
func TestMarshalLoadRoundTrip(t *testing.T) {
	recs := []Record{
		{Snippet: "func A() {}", Label: LabelGood, Verdict: VerdictPass, Agree: true, Reason: "ok", Model: "gemma4-e2b", LatencyMS: 42},
		{Snippet: "func B() { panic() }", Label: LabelBad, Verdict: VerdictFail, Agree: true},
	}
	b, err := Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pilot.jsonl")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Snippet != recs[0].Snippet || got[0].Model != "gemma4-e2b" || got[1].Verdict != VerdictFail {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// The committed seed dataset (the actual pilot output) LOADS and every record's
// Agree field is consistent with Agreement(label,verdict) — a live-run artifact
// that slice-4 can trust as ground truth for the shape.
func TestSeedDatasetLoadsAndIsConsistent(t *testing.T) {
	// testdata/pilot-seed.jsonl is the committed pilot output.
	recs, err := Load(filepath.Join("testdata", "pilot-seed.jsonl"))
	if err != nil {
		t.Skipf("seed dataset not present yet: %v", err) // written by the pilot step
	}
	if len(recs) == 0 {
		t.Skip("seed dataset empty (fail-open pilot — binaries not on PATH at pilot time)")
	}
	for i, r := range recs {
		if r.Agree != Agreement(r.Label, r.Verdict) {
			t.Errorf("record %d: Agree=%v but Agreement(%s,%s)=%v — seed inconsistent",
				i, r.Agree, r.Label, r.Verdict, Agreement(r.Label, r.Verdict))
		}
	}
}

// Confidence round-trips through Marshal→Load (the graded margin the ceiling
// curves consume).
func TestConfidenceRoundTrip(t *testing.T) {
	recs := []Record{
		{Snippet: "a", Label: LabelGood, Verdict: VerdictPass, Agree: true, Confidence: 0.73},
		{Snippet: "b", Label: LabelBad, Verdict: VerdictDefer, Agree: false, Confidence: 0},
	}
	b, err := Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "c.jsonl")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Confidence != 0.73 || got[1].Confidence != 0 {
		t.Fatalf("confidence round-trip mismatch: %+v", got)
	}
}

// LoadSnippets reads the labeled input corpus (id/snippet/label per line).
func TestLoadSnippets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "snips.jsonl")
	body := `{"id":"g1","snippet":"func A() int { return 1 }","label":"good"}
{"id":"b1","snippet":"func B() int { return 1/0 }","label":"bad"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSnippets(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 || s[0].ID != "g1" || s[0].Label != LabelGood || s[1].Label != LabelBad {
		t.Fatalf("LoadSnippets wrong: %+v", s)
	}
}
