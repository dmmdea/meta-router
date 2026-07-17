package quotasig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

var now = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

const fullDrop = `{"model":{"id":"claude-opus-4-8"},"rate_limits":{"five_hour":{"used_percentage":12.5,"resets_at":1783726800},"seven_day":{"used_percentage":40.1,"resets_at":1784100000}}}`

func TestParseDropFull(t *testing.T) {
	obs, err := ParseDrop([]byte(fullDrop))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("want 2 observations, got %+v", obs)
	}
	if obs[0].Window != ledger.Win5h || obs[0].UsedPct != 12.5 {
		t.Fatalf("five_hour mis-parsed: %+v", obs[0])
	}
	if obs[0].ResetsAt.Unix() != 1783726800 {
		t.Fatalf("resets_at epoch mis-parsed: %v", obs[0].ResetsAt)
	}
}

// Each window may be independently absent (fact refresh §3).
func TestParseDropPartial(t *testing.T) {
	obs, err := ParseDrop([]byte(`{"rate_limits":{"seven_day":{"used_percentage":55,"resets_at":1784100000}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Window != ledger.Win7d || obs[0].UsedPct != 55 {
		t.Fatalf("partial drop mis-parsed: %+v", obs)
	}
}

func TestParseDropGarbage(t *testing.T) {
	if _, err := ParseDrop([]byte("not json")); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestIngestMissingFileFailsOpen(t *testing.T) {
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	n, err := Ingest(l, filepath.Join(t.TempDir(), "nope.json"), "claude", now)
	if err != nil || n != 0 {
		t.Fatalf("missing drop must fail open (0, nil), got (%d, %v)", n, err)
	}
}

func TestIngestObservesProvider(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "statusline-drop.json")
	if err := os.WriteFile(drop, []byte(fullDrop), 0o644); err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	n, err := Ingest(l, drop, "claude", now)
	if err != nil || n != 2 {
		t.Fatalf("want 2 ingested, got (%d, %v)", n, err)
	}
	b, ok := l.Bucket("claude", ledger.Win7d)
	if !ok || b.Source != "provider" || b.UsedPct != 40.1 {
		t.Fatalf("ingest must feed ObserveProvider: %+v", b)
	}
}

// A window with NO resets_at (schema drift) must be skipped: a zero-reset
// provider bucket never rolls and could lock the lane exhausted forever.
func TestIngestSkipsZeroResetWindows(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "statusline-drop.json")
	noReset := `{"rate_limits":{"five_hour":{"used_percentage":99}}}`
	if err := os.WriteFile(drop, []byte(noReset), 0o644); err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	n, err := Ingest(l, drop, "claude", now)
	if err != nil || n != 0 {
		t.Fatalf("zero-reset observation must be skipped: (%d, %v)", n, err)
	}
	if _, ok := l.Bucket("claude", ledger.Win5h); ok {
		t.Fatal("zero-reset window must not create a provider bucket")
	}
}

// RS2: changed observations append to the scarcity trace; repeats do not.
func TestIngestTracedAppendsOnChangeOnly(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "statusline-drop.json")
	trace := filepath.Join(dir, "quota-trace.jsonl")
	if err := os.WriteFile(drop, []byte(fullDrop), 0o644); err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	if _, _, err := IngestTraced(l, drop, trace, "claude", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IngestTraced(l, drop, trace, "claude", now.Add(time.Minute)); err != nil {
		t.Fatal(err) // identical values: no new trace rows
	}
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if lines := len(splitNonEmpty(string(b))); lines != 2 {
		t.Fatalf("want 2 trace rows (5h+7d, first ingest only), got %d:\n%s", lines, b)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// A window whose resets_at is already in the past is STALE — skip it and let
// the shadow floor govern rather than reporting a dead percentage as fresh.
func TestIngestSkipsStaleWindows(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "statusline-drop.json")
	stale := `{"rate_limits":{"five_hour":{"used_percentage":90,"resets_at":100},"seven_day":{"used_percentage":40,"resets_at":1784100000}}}`
	if err := os.WriteFile(drop, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	n, err := Ingest(l, drop, "claude", now)
	if err != nil || n != 1 {
		t.Fatalf("stale five_hour must be skipped: (%d, %v)", n, err)
	}
	if _, ok := l.Bucket("claude", ledger.Win5h); ok {
		t.Fatal("stale window must not create a provider bucket")
	}
}
