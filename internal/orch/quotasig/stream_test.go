package quotasig

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// The committed live capture (CLI 2.1.199) — its single rate_limit_event line:
// status "allowed", rateLimitType "five_hour", resetsAt 1783312200
// (2026-07-06T04:30:00Z), and an overageStatus of "rejected" that is the
// OVERAGE billing state, NOT the window status. This test is the pin that the
// parser never confuses the two: if overageStatus were read as the status, the
// fixture would exhaust instead of anchor.
func TestIngestStreamEventsFromLiveFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", "claude", "stream-events-sonnet.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if n := IngestStreamEvents(l, b, "claude", now); n != 1 {
		t.Fatalf("fixture carries exactly one rate_limit_event, ingested %d", n)
	}
	bkt, ok := l.Bucket("claude", ledger.Win5h)
	if !ok || !bkt.ResetsAt.Equal(time.Unix(1783312200, 0).UTC()) {
		t.Fatalf("status=allowed must anchor the provider-true reset, not exhaust: %+v", bkt)
	}
	if bkt.UsedPct != -1 || bkt.Source == "provider" {
		t.Fatalf("allowed events carry no percentage — must not fabricate one: %+v", bkt)
	}
}

func TestIngestStreamExhaustionStatus(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1783312200,"rateLimitType":"five_hour"}}`
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	IngestStreamEvents(l, []byte(line), "claude", time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC))
	if b, _ := l.Bucket("claude", ledger.Win5h); b.UsedPct != 100 || b.Source != "provider" {
		t.Fatalf("non-allowed status is a sanctioned exhaustion signal: %+v", b)
	}
}

// S2R-7: stream ingest fails OPEN — only the KNOWN-exhausted status set
// observes 100%; an unknown status is schema drift and must be skipped exactly
// like an unknown rateLimitType, never guessed into an exhaustion.
func TestIngestStreamUnknownStatusSkipped(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning_v2","resetsAt":1783312200,"rateLimitType":"five_hour"}}`
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if n := IngestStreamEvents(l, []byte(line), "claude", time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)); n != 0 {
		t.Fatalf("unknown status must be skipped (schema drift, fail open), ingested %d", n)
	}
	if _, ok := l.Bucket("claude", ledger.Win5h); ok {
		t.Fatal("a skipped event must not touch the ledger")
	}
}

func TestIngestStreamUnknownWindowSkipped(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1783312200,"rateLimitType":"one_hour"}}`
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if n := IngestStreamEvents(l, []byte(line), "claude", time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)); n != 0 {
		t.Fatalf("unknown rateLimitType must be skipped (schema drift), ingested %d", n)
	}
	if _, ok := l.Bucket("claude", ledger.Win5h); ok {
		t.Fatal("a skipped event must not touch the ledger")
	}
}

// S2R-7 staleness guard, matching Ingest: a reset already in the past (or
// absent) must be skipped — a dead reset must not anchor, and a dead
// exhaustion must not lock the lane.
func TestIngestStreamStaleOrZeroResetSkipped(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC) // AFTER the 04:30Z reset
	lines := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1783312200,"rateLimitType":"five_hour"}}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1783312200,"rateLimitType":"five_hour"}}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day"}}
`
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if n := IngestStreamEvents(l, []byte(lines), "claude", now); n != 0 {
		t.Fatalf("stale/zero resets must all be skipped, ingested %d", n)
	}
	if _, ok := l.Bucket("claude", ledger.Win5h); ok {
		t.Fatal("stale events must not create buckets")
	}
	if _, ok := l.Bucket("claude", ledger.Win7d); ok {
		t.Fatal("zero-reset events must not create buckets")
	}
}

// S2R-7 authoritative overwrite end-to-end: a provider-true reset from the
// stream REPLACES a self-anchored estimate; a full provider observation
// (percentage + reset from the statusline tee) stays untouched by the
// anchor-only signal.
func TestIngestStreamAuthoritativeOverwrite(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("claude", ledger.Win5h, 1_000, now) // self-anchored estimate now+5h
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1783312200,"rateLimitType":"five_hour"}}`
	if n := IngestStreamEvents(l, []byte(line), "claude", now); n != 1 {
		t.Fatalf("fresh allowed event must apply, ingested %d", n)
	}
	if b, _ := l.Bucket("claude", ledger.Win5h); !b.ResetsAt.Equal(time.Unix(1783312200, 0).UTC()) {
		t.Fatalf("provider-true reset must replace the estimate anchor: %v", b.ResetsAt)
	}
	// Provider-sourced bucket: the tee's observation outranks the anchor.
	teeReset := now.Add(2 * time.Hour)
	l.ObserveProvider("claude", ledger.Win5h, 42, teeReset, now)
	IngestStreamEvents(l, []byte(line), "claude", now)
	if b, _ := l.Bucket("claude", ledger.Win5h); !b.ResetsAt.Equal(teeReset) || b.UsedPct != 42 {
		t.Fatalf("a provider observation must survive the stream anchor: %+v", b)
	}
}

// Non-rate_limit_event lines (the stream is mostly system/assistant/result
// events) and garbage lines are ignored — the ingest reads ONE event type and
// fails open on everything else.
func TestIngestStreamIgnoresOtherEventTypes(t *testing.T) {
	lines := `{"type":"system","subtype":"init"}
not json at all
{"type":"result","subtype":"success"}
`
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if n := IngestStreamEvents(l, []byte(lines), "claude", time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)); n != 0 {
		t.Fatalf("no rate_limit_event ⇒ nothing ingested, got %d", n)
	}
}
