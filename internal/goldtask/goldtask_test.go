package goldtask

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gs.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndValidate(t *testing.T) {
	good := `{"id":"AC-04","class":"agentic-coding","split":"heldout","repo":"meta-router","prompt":"do the thing","verify":{"kind":"vgo","repo":"meta-router","parent":"cad3ada","resolving":"bc14411","test_files":["internal/index/refresh_test.go"],"pkgs":["./internal/index/..."],"pre_gate":[{"name":"diff","pattern":"diff --git"}]}}
{"id":"RS-03","class":"research","split":"tuning","repo":"local-offload","prompt":"evaluate fit","verify":{"kind":"pure","keys":[{"name":"verdict","pattern":"not a generator fit"}]}}
`
	ts, err := Load(writeTemp(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].ID != "AC-04" || ts[1].Verify.Kind != "pure" {
		t.Fatalf("load wrong: %+v", ts)
	}
	if err := Validate(ts); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}

	cases := []struct{ name, body string }{
		{"duplicate id", `{"id":"X","class":"research","split":"tuning","repo":"r","prompt":"p","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
{"id":"X","class":"research","split":"tuning","repo":"r","prompt":"p","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
`},
		{"bad split", `{"id":"X","class":"research","split":"exam","repo":"r","prompt":"p","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
`},
		{"bad class", `{"id":"X","class":"misc","split":"tuning","repo":"r","prompt":"p","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
`},
		{"vgo missing parent", `{"id":"X","class":"quick-edit","split":"tuning","repo":"r","prompt":"p","verify":{"kind":"vgo","repo":"r","resolving":"abc","pkgs":["./..."]}}
`},
		{"pure without keys", `{"id":"X","class":"research","split":"tuning","repo":"r","prompt":"p","verify":{"kind":"pure"}}
`},
		{"empty prompt", `{"id":"X","class":"research","split":"tuning","repo":"r","prompt":"","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
`},
	}
	for _, c := range cases {
		ts, err := Load(writeTemp(t, c.body))
		if err != nil {
			t.Fatalf("%s: load should not error (validation is Validate's job): %v", c.name, err)
		}
		if err := Validate(ts); err == nil {
			t.Errorf("%s: Validate accepted an invalid set", c.name)
		}
	}
}

// pairs that must share a split (contamination same-side gate from the locked
// selection doc).
var samSidePairs = [][2]string{
	{"AC-12", "RV-06"}, {"AC-16", "RV-03"}, {"AC-06", "EX-10"},
	{"AC-18", "EX-03"}, {"EX-01", "RV-08"}, {"RV-01", "RV-14"},
}

// TestRealGoldsetStructure is the executable form of the locked selection:
// counts, split sizes, and contamination pairs. Partial batches are tolerated
// until all 56 land (the full gates arm at finalCount).
func TestRealGoldsetStructure(t *testing.T) {
	const finalCount = 56
	ts, err := Load(filepath.Join("..", "..", "testdata", "routing-goldset.jsonl"))
	if err != nil {
		t.Skipf("routing goldset not authored yet: %v", err)
	}
	if err := Validate(ts); err != nil {
		t.Fatalf("goldset invalid: %v", err)
	}
	if len(ts) < finalCount {
		t.Logf("partial goldset: %d/%d records (full gates arm at %d)", len(ts), finalCount, finalCount)
		return
	}
	if len(ts) != finalCount {
		t.Fatalf("goldset has %d records, want exactly %d", len(ts), finalCount)
	}
	wantClass := map[string]int{"agentic-coding": 12, "quick-edit": 10, "research": 12, "extraction": 10, "review": 12}
	wantTuning := map[string]int{"agentic-coding": 7, "quick-edit": 6, "research": 7, "extraction": 6, "review": 7}
	gotClass, gotTuning := map[string]int{}, map[string]int{}
	split := map[string]string{}
	tuning := 0
	for _, x := range ts {
		gotClass[x.Class]++
		split[x.ID] = x.Split
		if x.Split == "tuning" {
			gotTuning[x.Class]++
			tuning++
		}
	}
	for c, n := range wantClass {
		if gotClass[c] != n {
			t.Errorf("class %s: %d records, want %d", c, gotClass[c], n)
		}
		if gotTuning[c] != wantTuning[c] {
			t.Errorf("class %s tuning: %d, want %d", c, gotTuning[c], wantTuning[c])
		}
	}
	if tuning != 33 || finalCount-tuning != 23 {
		t.Errorf("split totals %d/%d, want 33/23", tuning, finalCount-tuning)
	}
	for _, p := range samSidePairs {
		if split[p[0]] == "" || split[p[1]] == "" {
			t.Errorf("pair %v: member missing from goldset", p)
			continue
		}
		if split[p[0]] != split[p[1]] {
			t.Errorf("contamination pair %v straddles the split: %s vs %s", p, split[p[0]], split[p[1]])
		}
	}
}
