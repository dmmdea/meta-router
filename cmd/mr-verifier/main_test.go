package main

import (
	"testing"

	vp "github.com/dmmdea/meta-router/internal/orch/strategy/verifierpilot"
)

// The committed corpus loads, is non-trivially sized, balanced, id-unique, and
// only uses the two valid labels — so a torn or lopsided corpus fails the gate.
// Path is relative to this package dir (cmd/mr-verifier) → repo-root testdata.
func TestVerifierSnippetCorpus(t *testing.T) {
	s, err := vp.LoadSnippets("../../testdata/verifier-snippets.jsonl")
	if err != nil {
		t.Fatalf("corpus load: %v", err)
	}
	if len(s) < 20 {
		t.Fatalf("corpus too small: %d (want >=20 for a pilot)", len(s))
	}
	ids := map[string]bool{}
	var good, bad int
	for i, r := range s {
		if r.ID == "" || ids[r.ID] {
			t.Fatalf("record %d: empty or duplicate id %q", i, r.ID)
		}
		ids[r.ID] = true
		switch r.Label {
		case vp.LabelGood:
			good++
		case vp.LabelBad:
			bad++
		default:
			t.Fatalf("record %d: invalid label %q", i, r.Label)
		}
	}
	if good < 8 || bad < 8 {
		t.Fatalf("corpus lopsided: good=%d bad=%d (want both >=8)", good, bad)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantV    vp.Verdict
		wantConf float64
	}{
		{"yes->pass with margin", `{"ok":true,"result":{"decision":"yes"},"meta":{"margin":0.71,"model":"gemma4-e2b"}}`, vp.VerdictPass, 0.71},
		{"no->fail with margin", `{"ok":true,"result":{"decision":"no"},"meta":{"margin":0.42}}`, vp.VerdictFail, 0.42},
		{"unsure->defer conf0", `{"ok":true,"result":{"decision":"unsure"},"meta":{"margin":0.05}}`, vp.VerdictDefer, 0},
		{"deferred->defer conf0", `{"ok":false,"deferred":true,"reason":"low margin"}`, vp.VerdictDefer, 0},
		{"ok-false->error", `{"ok":false,"reason":"tier crash"}`, vp.VerdictErrored, 0},
		{"garbage->error", `not json`, vp.VerdictErrored, 0},
	}
	for _, c := range cases {
		gotV, gotConf, _ := parseVerdict([]byte(c.raw))
		if gotV != c.wantV || gotConf != c.wantConf {
			t.Errorf("%s: got (%s,%v) want (%s,%v)", c.name, gotV, gotConf, c.wantV, c.wantConf)
		}
	}
}
