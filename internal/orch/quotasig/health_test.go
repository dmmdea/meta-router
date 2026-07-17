package quotasig

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// E6: provider-sourced buckets whose ObservedAt is older than staleAfter are
// named loudly (the tee stopped / schema drifted); shadow buckets are exempt
// (self-generated, always "fresh" by construction, governed by the shadow floor).
func TestStaleBucketsNamesOnlyStaleProviderBuckets(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	bs := []ledger.Bucket{
		{Lane: "claude", Window: ledger.Win5h, Source: "provider", ObservedAt: now.Add(-60 * time.Hour)},
		{Lane: "claude", Window: ledger.Win7d, Source: "provider", ObservedAt: now.Add(-1 * time.Hour)},
		{Lane: "codex", Window: ledger.Win5h, Source: "shadow", ObservedAt: now.Add(-90 * time.Hour)},
		{Lane: "glm", Window: ledger.Win5h, Source: "provider"}, // zero ObservedAt: never observed -> skip (nothing to go stale)
	}
	got := StaleBuckets(bs, now, 48*time.Hour)
	if len(got) != 1 {
		t.Fatalf("exactly the 60h-old provider bucket must be flagged, got %v", got)
	}
	if want := "claude/5h"; len(got) == 1 && !strings.Contains(got[0], want) {
		t.Fatalf("flag must name lane/window %q, got %q", want, got[0])
	}
}
