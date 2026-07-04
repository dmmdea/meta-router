package retrievers

import (
	"testing"
	"time"
)

// TestDeadEndpointFailsFast verifies the MR-13 connect/total timeout split:
// a query against a blackholed endpoint must fail in roughly ConnectTimeout
// (~200ms), NOT the multi-second total budget — so mr-hook can log
// embedder-down (or run the BM25 fallback) well inside its own deadline.
// 203.0.113.1 is TEST-NET-3 (RFC 5737): reserved for documentation, never
// routable; the dial either times out at ConnectTimeout or is rejected
// immediately — both well under the 1.5s assertion.
func TestDeadEndpointFailsFast(t *testing.T) {
	e := NewEmbedFromVectors([]string{"a"}, [][]float64{{1, 0}}, "http://203.0.113.1:9", 5*time.Second)
	start := time.Now()
	_, _, err := e.RetrieveScored("some prompt", 1)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error against a dead endpoint")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("dead endpoint took %v; connect timeout split (200ms) not effective", elapsed)
	}
}
