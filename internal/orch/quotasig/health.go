package quotasig

import (
	"fmt"
	"sort"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// StaleBuckets (E6) names every provider-sourced bucket whose ObservedAt is
// older than staleAfter — the tee stopped dropping, or the statusline schema
// drifted past the ingest guards. Shadow buckets are exempt (self-generated;
// the shadow floor governs them structurally). Zero ObservedAt = never observed
// -> skipped (absence of a signal is the trace-health alarm's job, not this
// one's). Sorted for deterministic status output.
func StaleBuckets(bs []ledger.Bucket, now time.Time, staleAfter time.Duration) []string {
	var out []string
	for _, b := range bs {
		if b.Source != "provider" || b.ObservedAt.IsZero() {
			continue
		}
		if age := now.Sub(b.ObservedAt); age > staleAfter {
			out = append(out, fmt.Sprintf("%s/%s: provider signal stale %dh (observed %s)",
				b.Lane, b.Window, int(age.Hours()), b.ObservedAt.UTC().Format(time.RFC3339)))
		}
	}
	sort.Strings(out)
	return out
}
