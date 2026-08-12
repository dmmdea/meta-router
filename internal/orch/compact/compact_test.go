package compact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// W5 canary (lossless round-trip): Decompact(Compact(x)) must be semantically
// equal to x for every case — the DG-3 contract. Reverting the rotation, the
// marker guard, or the decode goes red here.
func TestRoundTripIsLossless(t *testing.T) {
	cases := []string{
		`{"a":1,"b":[1,2,3],"c":"x"}`,
		`[{"a":1,"b":"x"},{"a":2,"b":"y"},{"a":3,"b":"z"}]`,                             // rotates
		`[{"a":1},{"a":2}]`,                                                             // below minRows — no rotation
		`[{"a":1,"b":2},{"a":3},{"a":4,"b":5}]`,                                         // heterogeneous — no rotation
		`{"outer":[{"k":[{"x":1,"y":2},{"x":3,"y":4},{"x":5,"y":6}]},{"k":1},{"k":2}]}`, // nested rotation inside a non-rotating array
		`[{"a":null,"b":false},{"a":0,"b":true},{"a":"","b":true}]`,                     // zero values are values, not absences
		`[[1,2],[3,4],[5,6]]`,                                                           // arrays of arrays — never rotated
		`"just a string"`,
		`42`,
		`null`,
		`{}`,
		`[]`,
		`{"@columnar":{"cols":["a"],"rows":[[1]]}}`, // document SPEAKS the marker — minify only, decode untouched
		`{"deep":{"nested":[{"a":{"inner":[{"p":1,"q":2},{"p":3,"q":4},{"p":5,"q":6}]},"b":1},{"a":2,"b":3}]}}`,
	}
	for i, raw := range cases {
		out, _ := Compact(raw)
		back, ok := Decompact(out)
		if !ok {
			t.Fatalf("case %d: decompact failed on %q", i, out)
		}
		if !Equal(raw, back) {
			t.Fatalf("case %d NOT lossless:\nraw:  %s\nout:  %s\nback: %s", i, raw, out, back)
		}
	}
}

// The marker escape: a document already using "@columnar" (or "@@columnar",
// or any depth) still compacts, and its own keys come back EXACTLY — every
// bare "@columnar" in Compact's output is provably ours, so decode never
// guesses. The original encode-side skip was insufficient: Decompact of a
// passed-through document unrotated USER data (caught by the round-trip
// test).
func TestMarkerEscapeIsBijective(t *testing.T) {
	for _, raw := range []string{
		`{"@columnar":"mine","rows":[{"a":1,"b":2},{"a":3,"b":4},{"a":5,"b":6}]}`,
		`{"@columnar":{"cols":["a"],"rows":[[1]]}}`, // user data SHAPED like our marker
		`{"@@columnar":1,"@@@columnar":[2]}`,        // pre-escaped family members
	} {
		out, _ := Compact(raw)
		back, ok := Decompact(out)
		if !ok || !Equal(raw, back) {
			t.Fatalf("marker-family document must round-trip exactly:\nraw:  %s\nout:  %s\nback: %s", raw, out, back)
		}
	}
}

// Rotation actually saves on the shape it targets (repeated keys), and the
// smaller form always wins.
func TestCompactionSaves(t *testing.T) {
	var rows []string
	for i := 0; i < 20; i++ {
		rows = append(rows, fmt.Sprintf(`{"timestamp":"2026-08-12T00:00:%02dZ","lane":"claude","outcome_class":"ok","tokens_in":%d}`, i, i*100))
	}
	raw := "[\n  " + strings.Join(rows, ",\n  ") + "\n]"
	out, applied := Compact(raw)
	if !applied || len(out) >= len(raw) {
		t.Fatalf("repeated-key rows must compact: %d -> %d", len(raw), len(out))
	}
	if !strings.Contains(out, `"@columnar"`) {
		t.Fatalf("this shape must rotate: %s", out[:80])
	}
	back, _ := Decompact(out)
	if !Equal(raw, back) {
		t.Fatal("savings must never cost fidelity")
	}
	// And the savings are real on this archetype (>40% on 20 rows).
	if saved := len(raw) - len(out); saved*100/len(raw) < 40 {
		t.Fatalf("expected >40%% savings on the repeated-key archetype, got %d%%", saved*100/len(raw))
	}
}

// Non-JSON is never touched — prose compaction would be LOSSY by definition.
func TestNonJSONUntouched(t *testing.T) {
	if out, applied := Compact("a prose answer, not JSON {maybe"); applied || out != "" {
		t.Fatalf("prose must never compact: %q %v", out, applied)
	}
}

// A round-trip preserves number fidelity through the any-decode (float64
// canonicalization is shared by both sides of Equal, so 1 vs 1.0 stays equal
// while 1 vs 2 stays unequal).
func TestEqualJudgesSemantics(t *testing.T) {
	if !Equal(`{"a": 1, "b": [1,2]}`, `{"b":[1,2],"a":1}`) {
		t.Fatal("key order and whitespace are not semantic")
	}
	if Equal(`{"a":1}`, `{"a":2}`) {
		t.Fatal("different values must not be equal")
	}
	var big []map[string]any
	for i := 0; i < 5; i++ {
		big = append(big, map[string]any{"v": float64(i) / 3.0, "s": strings.Repeat("x", i)})
	}
	b, _ := json.Marshal(big)
	out, _ := Compact(string(b))
	back, _ := Decompact(out)
	if !Equal(string(b), back) {
		t.Fatal("float fidelity must survive the rotation")
	}
}
