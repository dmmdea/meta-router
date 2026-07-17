package goldtask

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/meta-router/internal/oracle"
)

func TestPureCheck(t *testing.T) {
	pure := VerifySpec{Kind: "pure", Keys: []Key{
		{Name: "a", Pattern: "llama-swap v230"},
		{Name: "b", Pattern: "#23677"},
		{Name: "c", Pattern: "embeddinggemma.{0,40}keep"},
	}}
	cases := []struct {
		name string
		spec VerifySpec
		out  string
		want bool
	}{
		{"all keys present", pure, "Latest is llama-swap v230; bug #23677 open; EmbeddingGemma verdict: KEEP.", true},
		{"one key missing", pure, "llama-swap v230 and #23677 only", false},
		{"empty output never passes", pure, "", false},
		{"case-insensitive", pure, "LLAMA-SWAP V230 ... #23677 ... embeddinggemma: keep", true},
		{"minkeys subset", VerifySpec{Kind: "pure", MinKeys: 2, Keys: pure.Keys}, "llama-swap v230 and #23677 only", true},
		{"minkeys not met", VerifySpec{Kind: "pure", MinKeys: 2, Keys: pure.Keys}, "only llama-swap v230 here", false},
		{"forbidden vetoes", VerifySpec{Kind: "pure", Keys: pure.Keys[:1],
			Forbidden: []Key{{Name: "f", Pattern: "adopt ik_llama"}}},
			"llama-swap v230; we should adopt ik_llama", false},
		{"exec kind uses pregate", VerifySpec{Kind: "vgo", PreGate: []Key{
			{Name: "diff", Pattern: `(^|\n)diff --git`}, {Name: "hunk", Pattern: `(^|\n)@@ [^\n]+ @@`}}},
			"diff --git a/x.go b/x.go\n@@ -1,2 +1,2 @@\n-old\n+new", true},
		{"exec pregate rejects prose", VerifySpec{Kind: "vgo", PreGate: []Key{
			{Name: "diff", Pattern: `(^|\n)diff --git`}}},
			"I have carefully made the change and all tests pass.", false},
	}
	for _, c := range cases {
		if got := PureCheck(c.spec, c.out); got != c.want {
			t.Errorf("%s: PureCheck=%v want %v", c.name, got, c.want)
		}
	}
}

func TestCompileErr(t *testing.T) {
	if err := CompileErr(VerifySpec{Kind: "pure", Keys: []Key{{Name: "ok", Pattern: "a+b"}}}); err != nil {
		t.Fatalf("valid pattern rejected: %v", err)
	}
	if err := CompileErr(VerifySpec{Kind: "pure", Keys: []Key{{Name: "bad", Pattern: "([unclosed"}}}); err == nil {
		t.Fatal("invalid pattern accepted")
	}
	if err := CompileErr(VerifySpec{Kind: "vgo", PreGate: []Key{{Name: "bad", Pattern: "([unclosed"}}}); err == nil {
		t.Fatal("invalid pre_gate pattern accepted")
	}
}

// TestV0AuditRealGoldset is the slice-0 fake-green killer applied to the whole
// gold set: every task's pure face (keys or pre-gate) must reject the null
// agent and every trivial-slop output, and every pattern must compile.
func TestV0AuditRealGoldset(t *testing.T) {
	ts, err := Load(filepath.Join("..", "..", "testdata", "routing-goldset.jsonl"))
	if err != nil {
		t.Skipf("routing goldset not authored yet: %v", err)
	}
	for _, x := range ts {
		if err := CompileErr(x.Verify); err != nil {
			t.Errorf("%s: pattern does not compile: %v", x.ID, err)
		}
	}
	faults := oracle.Audit(RegisterAll(ts))
	for _, f := range faults {
		out := f.Output
		if len(out) > 60 {
			out = out[:60] + "…"
		}
		t.Errorf("V0 audit fault: verifier %s accepted degenerate output %q", f.VerifierID, out)
	}
}
