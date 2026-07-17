package calib

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

func mk(pct float64, shadow int64) Sample {
	return Sample{Lane: "claude", Window: "7d", UsedPct: pct, ShadowTokens: shadow}
}

func TestFitRequiresAgreement(t *testing.T) {
	agree := []Sample{mk(25, 250_000), mk(30, 310_000), mk(40, 390_000), mk(50, 505_000), mk(60, 590_000)}
	capTok, n, ok := Fit(agree, "claude", ledger.Win7d, Defaults())
	if !ok || n < 5 || capTok < 950_000 || capTok > 1_050_000 { // all ≈1M within 15%
		t.Fatalf("agreeing samples must fit ~1M: cap=%d n=%d ok=%v", capTok, n, ok)
	}
	if _, _, ok := Fit(agree[:3], "claude", ledger.Win7d, Defaults()); ok {
		t.Fatal("under MinSamples must not fit")
	}
	noisy := append(agree[:4:4], mk(30, 3_000_000)) // wild outlier breaks agreement count
	if _, _, ok := Fit(noisy, "claude", ledger.Win7d, Defaults()); ok {
		t.Fatal("disagreeing samples must not fit")
	}
	low := []Sample{mk(5, 50_000), mk(8, 80_000), mk(10, 99_000), mk(12, 120_000), mk(15, 150_000)}
	if _, _, ok := Fit(low, "claude", ledger.Win7d, Defaults()); ok {
		t.Fatal("sub-MinPct rows are noise (tiny pct ⇒ huge relative error) and must not fit")
	}
}

// Fit must only consume rows for the requested (lane, window) — a mixed trace
// (claude 5h+7d interleaved) must not cross-contaminate the estimates.
func TestFitFiltersLaneAndWindow(t *testing.T) {
	samples := []Sample{
		mk(25, 250_000), mk(30, 310_000), mk(40, 390_000), mk(50, 505_000), mk(60, 590_000),
		{Lane: "claude", Window: "5h", UsedPct: 50, ShadowTokens: 9_000_000}, // wrong window
		{Lane: "glm", Window: "7d", UsedPct: 50, ShadowTokens: 40},           // wrong lane
	}
	capTok, n, ok := Fit(samples, "claude", ledger.Win7d, Defaults())
	if !ok || n != 5 || capTok < 950_000 || capTok > 1_050_000 {
		t.Fatalf("foreign rows must be excluded: cap=%d n=%d ok=%v", capTok, n, ok)
	}
	if _, _, ok := Fit(samples, "claude", ledger.Win5h, Defaults()); ok {
		t.Fatal("a single 5h row is far under MinSamples and must not fit")
	}
}

// Load is fail-open: missing file ⇒ nil; a torn/corrupt line is skipped
// without voiding the readable history. The JSON shape pinned here is the
// quotasig traceRow verbatim (ts/lane/window/used_pct/resets_at/shadow_tokens).
func TestLoadFailOpenAndTraceRowShape(t *testing.T) {
	if got := Load(filepath.Join(t.TempDir(), "nope.jsonl")); got != nil {
		t.Fatalf("missing trace must be nil, got %+v", got)
	}
	p := filepath.Join(t.TempDir(), "quota-trace.jsonl")
	content := `{"ts":"2026-07-06T12:00:00Z","lane":"claude","window":"7d","used_pct":40.1,"resets_at":"2026-07-13T10:00:00Z","shadow_tokens":50000}
{not json — torn append
{"ts":"2026-07-06T13:00:00Z","lane":"claude","window":"5h","used_pct":12.5,"resets_at":"2026-07-06T17:00:00Z","shadow_tokens":9000}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Load(p)
	if len(got) != 2 {
		t.Fatalf("want 2 parseable rows (torn line skipped), got %+v", got)
	}
	if got[0].Lane != "claude" || got[0].Window != "7d" || got[0].UsedPct != 40.1 || got[0].ShadowTokens != 50_000 {
		t.Fatalf("traceRow shape mis-parsed: %+v", got[0])
	}
	if got[0].TS.IsZero() {
		t.Fatalf("ts must parse (RFC3339): %+v", got[0])
	}
}
