package router

import "testing"

// "retired" (lane retirement, 2026-09-01) must MASK: masked() is a denylist,
// so a new state name silently reads as selectable unless listed — this pin
// exists because exactly that gap was caught at review-by-author time.
func TestRetiredStateIsMasked(t *testing.T) {
	if !masked("retired") {
		t.Fatal("retired must be a masked state")
	}
	if masked("open") || masked("throttled") {
		t.Fatal("open/throttled must not be masked")
	}
}
