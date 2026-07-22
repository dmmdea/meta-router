package spenddown

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

var t0 = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// bucket returns a provider-sourced 5h bucket resetting in `toReset`, used at pct.
func bucket(pct float64, toReset time.Duration) ledger.Bucket {
	return ledger.Bucket{Lane: "claude", Window: ledger.Win5h, UsedPct: pct,
		ResetsAt: t0.Add(toReset), Source: "provider"}
}

// samples fabricates a same-epoch trace: usedAgo at t0-ago, usedNow at t0.
func samples(usedAgo, usedNow float64, ago time.Duration) []calib.Sample {
	return []calib.Sample{
		{TS: t0.Add(-ago), Lane: "claude", Window: "5h", UsedPct: usedAgo},
		{TS: t0, Lane: "claude", Window: "5h", UsedPct: usedNow},
	}
}

// A near-reset, under-utilized, provider-sourced window arms the latch at 1.
func TestArmsOnUnderUtilizedNearReset(t *testing.T) {
	e := Assess(samples(9, 10, 15*time.Minute), bucket(10, time.Hour), Entry{}, t0, Defaults())
	if e.Level != 1 {
		t.Fatalf("want level 1, got %+v", e)
	}
	if !e.ChangedAt.Equal(t0) {
		t.Fatalf("ChangedAt must stamp the transition, got %v", e.ChangedAt)
	}
}

// Far from reset (outside Horizon) the latch stays 0 no matter how idle.
func TestNoArmOutsideHorizon(t *testing.T) {
	if e := Assess(samples(9, 10, 15*time.Minute), bucket(10, 3*time.Hour), Entry{}, t0, Defaults()); e.Level != 0 {
		t.Fatalf("outside horizon must not arm, got %+v", e)
	}
}

// The trigger is the PROJECTED end-of-window unused fraction, not the
// instantaneous pct: a window at 20% used but burning 60pct/h projects past the
// floor and must not arm.
func TestForecastBlocksFastBurner(t *testing.T) {
	// 45→60 over 15min = 60 pct/h; 1h to reset → projected 120 (clamped 100) → unused 0.
	// Use sub-Raise numbers so ONLY the forecast can block: 5→20 over 15min at 20% used.
	e := Assess(samples(5, 20, 15*time.Minute), bucket(20, time.Hour), Entry{}, t0, Defaults())
	if e.Level != 0 {
		t.Fatalf("projected-exhausting window must not arm, got %+v", e)
	}
}

// Hysteresis: between Raise (25) and Drop (35) an armed latch HOLDS and a
// disarmed latch stays disarmed.
func TestHysteresisBand(t *testing.T) {
	prev := Entry{Level: 1, ChangedAt: t0.Add(-time.Hour)}
	if e := Assess(samples(29, 30, 15*time.Minute), bucket(30, time.Hour), prev, t0, Defaults()); e.Level != 1 {
		t.Fatalf("armed latch must hold in the band, got %+v", e)
	}
	if e := Assess(samples(29, 30, 15*time.Minute), bucket(30, time.Hour), Entry{}, t0, Defaults()); e.Level != 0 {
		t.Fatalf("disarmed latch must not arm in the band, got %+v", e)
	}
}

// Above DropPct the latch drops immediately (no cooldown on the safety
// direction).
func TestDropAboveDropPct(t *testing.T) {
	prev := Entry{Level: 2, ChangedAt: t0.Add(-time.Second)}
	if e := Assess(samples(39, 40, 15*time.Minute), bucket(40, time.Hour), prev, t0, Defaults()); e.Level != 0 {
		t.Fatalf("above DropPct must disarm immediately, got %+v", e)
	}
}

// The ramp: an armed latch escalates one level per elapsed cooldown, bounded at
// MaxBoost; inside the cooldown it holds.
func TestRampCooldownAndBound(t *testing.T) {
	opt := Defaults()
	armed := Entry{Level: 1, ChangedAt: t0.Add(-opt.Cooldown - time.Second)}
	e := Assess(samples(9, 10, 15*time.Minute), bucket(10, time.Hour), armed, t0, opt)
	if e.Level != 2 {
		t.Fatalf("elapsed cooldown must ramp 1→2, got %+v", e)
	}
	fresh := Entry{Level: 1, ChangedAt: t0.Add(-opt.Cooldown / 2)}
	if e := Assess(samples(9, 10, 15*time.Minute), bucket(10, time.Hour), fresh, t0, opt); e.Level != 1 {
		t.Fatalf("inside cooldown must hold, got %+v", e)
	}
	max := Entry{Level: opt.MaxBoost, ChangedAt: t0.Add(-opt.Cooldown - time.Second)}
	if e := Assess(samples(9, 10, 15*time.Minute), bucket(10, time.Hour), max, t0, opt); e.Level != opt.MaxBoost {
		t.Fatalf("ramp must bound at MaxBoost, got %+v", e)
	}
}

// Cooldown also gates the initial arm when a recent change is on record
// (anti-oscillation: rapid disarm→re-arm flapping).
func TestArmRespectsCooldown(t *testing.T) {
	opt := Defaults()
	justDropped := Entry{Level: 0, ChangedAt: t0.Add(-opt.Cooldown / 2)}
	if e := Assess(samples(9, 10, 15*time.Minute), bucket(10, time.Hour), justDropped, t0, opt); e.Level != 0 {
		t.Fatalf("re-arm inside cooldown must hold at 0, got %+v", e)
	}
}

// Disqualifiers: unknown pct, unanchored, past reset, estimate-sourced.
func TestDisqualifiers(t *testing.T) {
	s := samples(9, 10, 15*time.Minute)
	cases := map[string]ledger.Bucket{
		"unknown pct": {Lane: "claude", Window: ledger.Win5h, UsedPct: -1, ResetsAt: t0.Add(time.Hour), Source: "provider"},
		"unanchored":  {Lane: "claude", Window: ledger.Win5h, UsedPct: 10, Source: "provider"},
		"past reset":  {Lane: "claude", Window: ledger.Win5h, UsedPct: 10, ResetsAt: t0.Add(-time.Minute), Source: "provider"},
		"estimate-sourced": {Lane: "claude", Window: ledger.Win5h, UsedPct: 10,
			ResetsAt: t0.Add(time.Hour), Source: "shadow", CapSource: ledger.CapSourceEstimate},
	}
	for name, b := range cases {
		if e := Assess(s, b, Entry{Level: 1, ChangedAt: t0.Add(-time.Hour)}, t0, Defaults()); e.Level != 0 {
			t.Fatalf("%s must disarm, got %+v", name, e)
		}
	}
}

// A trace whose newest row predates the bucket's budget epoch (row pct >
// bucket pct) must not feed the average or rate — fall back to the bucket's
// own measured pct (rate 0).
func TestStaleEpochTraceFallsBackToBucket(t *testing.T) {
	stale := samples(85, 90, 15*time.Minute) // pre-reset epoch rows
	if e := Assess(stale, bucket(10, time.Hour), Entry{}, t0, Defaults()); e.Level != 1 {
		t.Fatalf("stale-epoch trace must fall back to the bucket pct and arm, got %+v", e)
	}
}

// With no usable trace at all, the bucket's own measured pct decides (rate 0).
func TestNoTraceUsesBucketPct(t *testing.T) {
	if e := Assess(nil, bucket(10, time.Hour), Entry{}, t0, Defaults()); e.Level != 1 {
		t.Fatalf("no trace: bucket pct must arm, got %+v", e)
	}
	if e := Assess(nil, bucket(60, time.Hour), Entry{}, t0, Defaults()); e.Level != 0 {
		t.Fatalf("no trace: high bucket pct must not arm, got %+v", e)
	}
}

// Completion-fit: the task must end Buffer before reset; an absent estimate
// (0) never fits — the gate stays closed without the data to clear it.
func TestFits(t *testing.T) {
	opt := Defaults()
	b := bucket(10, time.Hour)
	if !Fits(b, 30*time.Minute, t0, opt) {
		t.Fatal("30m task must fit a 1h-to-reset window with a 10m buffer")
	}
	if Fits(b, 55*time.Minute, t0, opt) {
		t.Fatal("55m task must NOT fit (55+10 > 60)")
	}
	if Fits(b, 0, t0, opt) {
		t.Fatal("unknown duration must not fit (fail-quiet)")
	}
}

// State round-trip: Save then Load; a missing file loads empty.
func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend-down.json")
	if got := LoadState(p); len(got) != 0 {
		t.Fatalf("missing file must load empty, got %v", got)
	}
	s := State{"claude|5h": Entry{Level: 2, ChangedAt: t0}}
	if err := SaveState(p, s); err != nil {
		t.Fatal(err)
	}
	got := LoadState(p)
	if got["claude|5h"].Level != 2 || !got["claude|5h"].ChangedAt.Equal(t0) {
		t.Fatalf("round-trip mismatch: %v", got)
	}
	// Corrupt file fails open to empty.
	if err := os.WriteFile(p, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadState(p); len(got) != 0 {
		t.Fatalf("corrupt file must load empty, got %v", got)
	}
}

// Option hygiene: an inverted hysteresis band (Raise >= Drop) is hand-edit
// damage — both fall back to defaults.
func TestNormalizeInvertedBand(t *testing.T) {
	opt := Defaults()
	opt.RaisePct, opt.DropPct = 50, 40
	n := Normalize(opt)
	d := Defaults()
	if n.RaisePct != d.RaisePct || n.DropPct != d.DropPct {
		t.Fatalf("inverted band must reset to defaults, got %+v", n)
	}
}
