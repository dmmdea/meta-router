package quotasig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/quotapoll"
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

func TestApplySnapshotsWritesProviderAndTaggedTrace(t *testing.T) {
	dir := t.TempDir()
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	trace := filepath.Join(dir, "trace.jsonl")
	now := time.Now().UTC()
	snaps := []quotapoll.Snapshot{{Lane: "claude", Window: ledger.Win5h, UsedPct: 18, ResetsAt: now.Add(3 * time.Hour)}}
	ApplySnapshots(l, snaps, trace, "oauth_poll", now)
	ApplySnapshots(l, snaps, trace, "oauth_poll", now.Add(time.Minute)) // unchanged → no second row
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 trace row, got %d: %s", len(lines), b)
	}
	if !strings.Contains(lines[0], `"origin":"oauth_poll"`) {
		t.Fatalf("trace row must carry origin, got %s", lines[0])
	}
	bk, ok := l.Bucket("claude", ledger.Win5h)
	if !ok || bk.Source != "provider" || bk.UsedPct != 18 {
		t.Fatalf("snapshot must land as provider observation, got %+v", bk)
	}
}

func TestApplySnapshotsSkipsStale(t *testing.T) {
	dir := t.TempDir()
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	now := time.Now().UTC()
	n, _ := ApplySnapshots(l, []quotapoll.Snapshot{{Lane: "claude", Window: ledger.Win5h, UsedPct: 40, ResetsAt: now.Add(-time.Hour)}}, "", "oauth_poll", now)
	if n != 0 {
		t.Fatalf("stale reset must be skipped, applied %d", n)
	}
}

// REGRESSION (audit 2026-07-25): a 2.5-day-old hand-seeded drop file whose 7d
// anchor lay in the FUTURE was re-ingested as fresh provider truth on every
// route/run/mcp call, overwriting the real vendor-polled 17% with a fixture's
// 39% and re-stamping ObservedAt (which blinded the E6 staleness alarm).
func TestIngestSkipsStaleDropFile(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "statusline-drop.json")
	// The exact live payload: 5h anchor already past, 7d anchor in the future.
	body := `{"rate_limits":{"five_hour":{"used_percentage":11.0,"resets_at":1784785799},"seven_day":{"used_percentage":39.0,"resets_at":1785312000}}}`
	if err := os.WriteFile(drop, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-50 * time.Hour)
	if err := os.Chtimes(drop, stale, stale); err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	n, note, err := IngestTraced(l, drop, filepath.Join(dir, "trace.jsonl"), "claude", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a 50h-old drop must contribute no observations, applied %d", n)
	}
	if note == "" || !strings.Contains(note, "old") {
		t.Fatalf("skipping a stale drop must be STATED, got note=%q", note)
	}
	if _, ok := l.Bucket("claude", ledger.Win7d); ok {
		t.Fatal("stale drop must not create a bucket")
	}
}

// A statusline drop must never overwrite a LIVE vendor-polled observation:
// they disagreed by 22pp and 14h of anchor in the same instant on the live box.
func TestDropNeverOverwritesLivePoll(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	// Vendor poll lands first: the real number.
	if n, _ := ApplySnapshotsSubject(l, "", []quotapoll.Snapshot{
		{Lane: "claude", Window: ledger.Win7d, UsedPct: 17, ResetsAt: now.Add(96 * time.Hour)},
	}, "", "oauth_poll", now); n != 1 {
		t.Fatal("poll snapshot must apply")
	}
	// A FRESH drop with a different number must be refused for that window.
	drop := filepath.Join(dir, "statusline-drop.json")
	future := now.Add(96 * time.Hour).Unix()
	body := fmt.Sprintf(`{"rate_limits":{"seven_day":{"used_percentage":39.0,"resets_at":%d}}}`, future)
	if err := os.WriteFile(drop, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	IngestTraced(l, drop, "", "claude", now)
	b, _ := l.Bucket("claude", ledger.Win7d)
	if b.UsedPct != 17 {
		t.Fatalf("a drop must not overwrite a live vendor poll: got %v want 17", b.UsedPct)
	}
}

// The trace row's TS must be the DROP's observation time, not the ingest
// instant — otherwise quota-parity pairs a drop against a poll with identical
// timestamps and reports a fabricated agreement.
func TestDropTraceRowCarriesObservationTime(t *testing.T) {
	dir := t.TempDir()
	drop := filepath.Join(dir, "statusline-drop.json")
	now := time.Now().UTC()
	obs := now.Add(-10 * time.Minute)
	body := fmt.Sprintf(`{"rate_limits":{"seven_day":{"used_percentage":39.0,"resets_at":%d}}}`, now.Add(96*time.Hour).Unix())
	if err := os.WriteFile(drop, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(drop, obs, obs); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, "trace.jsonl")
	l := ledger.Open(filepath.Join(dir, "ledger.json"))
	if n, _, _ := IngestTraced(l, drop, trace, "claude", now); n != 1 {
		t.Fatalf("a 10-minute-old drop is still live and must apply")
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	var row TraceRow
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(raw)), "\n")[0]), &row); err != nil {
		t.Fatal(err)
	}
	if d := row.TS.Sub(obs); d > time.Second || d < -time.Second {
		t.Fatalf("trace TS must be the drop's observation time %v, got %v", obs, row.TS)
	}
}
