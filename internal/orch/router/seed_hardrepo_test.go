package router

import "testing"

// A `--exclude claude` consult on the quality-first default class must have a
// measured lane to fall to. With GLM retired by config, HardRepo carried only
// claude + glm rows and delegate-mode deferred with "all lanes masked"
// (finding 2026-09-01). This pins a non-claude, non-glm row - earned by
// measurement, ranked below the SWE-V rows, never above them.
func TestSeedHardRepoHasMeasuredNonClaudeNonGLMFallback(t *testing.T) {
	rows := Seed()[HardRepo]
	var fallback *Entry
	for i := range rows {
		if rows[i].Lane != "claude" && rows[i].Lane != "glm" {
			fallback = &rows[i]
			break
		}
	}
	if fallback == nil {
		t.Fatal("HardRepo seed has no non-claude, non-glm row: --exclude claude defers with GLM retired")
	}
	for _, r := range rows {
		if r.Lane == "claude" && r.Rank >= fallback.Rank {
			t.Fatalf("fallback %s rank %d must sit below every claude row (claude rank %d)", fallback.Lane, fallback.Rank, r.Rank)
		}
	}
	if len(fallback.Evidence) < 20 {
		t.Fatalf("fallback row must cite measured evidence, got %q", fallback.Evidence)
	}
}
