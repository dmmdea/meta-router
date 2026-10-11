package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/dmmdea/meta-router/internal/retrievers"
)

// F42: the embedder behind the model router is unloaded after five idle minutes and needs
// seconds to load, but the hook's whole deadline is one second. Embedding into a cold router
// therefore waited out the embed budget, was cancelled, and fell back to BM25 anyway. The
// retriever below asks the router first (retrievers.ProbeEmbedder) and, when the model is not
// loaded, returns errColdEmbedder at once so decide() takes its BM25 branch, after starting the
// load with ONE warm-up request that does not hold the hook open.
//
// The three numbers are CHOSEN, not fitted. The router answers /running in ~1 ms on loopback,
// so 100 ms is a ceiling that a healthy router never approaches and a wedged one cannot
// spend the deadline with; the warm-up needs only the request on the wire (loopback: well
// under a millisecond) and 50 ms covers a slow dial without ever approaching the 950 ms wait
// it replaces.
const (
	runningProbeCap = 100 * time.Millisecond
	warmUpHold      = 0 * time.Millisecond
	warmUpDeadline  = 50 * time.Millisecond
)

// errColdEmbedder marks "skipped the embed wait because the model is not loaded". decide()
// matches it with errors.Is to log cold-skip-* instead of embedder-down: the embedder is not
// down, it is unloaded.
var errColdEmbedder = errors.New("embedder not loaded (cold): skipped the embed wait")

// hookBudgets splits the hard deadline. The embed call keeps everything but the 50 ms the
// surfacing needs AND the probe's ceiling, so probe + embed can never overrun the deadline
// (without that, a slow-but-ready /running would push a warm embed past it). With the cold
// skip off the arithmetic is exactly what it was.
func hookBudgets(timeoutMs int, coldSkip bool) (embedTO, probeTO time.Duration) {
	embedTO = time.Duration(timeoutMs) * time.Millisecond
	if timeoutMs > 50 {
		embedTO = time.Duration(timeoutMs-50) * time.Millisecond
	}
	if !coldSkip {
		return embedTO, 0
	}
	probeTO = min(runningProbeCap, embedTO/5)
	return embedTO - probeTO, probeTO
}

// coldSkipRetriever wraps the primary ranker (embed, rerank or hybrid: all three need the
// embedder). probe and warm are fields so the decision is testable without a router.
type coldSkipRetriever struct {
	inner scoredRetriever
	probe func() retrievers.EmbedProbe
	warm  func(target string) error
}

func newColdSkip(inner scoredRetriever, endpointSpec, model string, probeTO time.Duration) *coldSkipRetriever {
	return &coldSkipRetriever{
		inner: inner,
		probe: func() retrievers.EmbedProbe { return retrievers.ProbeEmbedder(endpointSpec, model, probeTO) },
		warm: func(target string) error {
			return retrievers.WarmUpEmbedder(target, model, warmUpHold, warmUpDeadline)
		},
	}
}

func (c *coldSkipRetriever) RetrieveScored(prompt string, k int) ([]retrievers.Scored, float64, error) {
	p := c.probe()
	if p.State != retrievers.EmbedCold {
		return c.inner.RetrieveScored(prompt, k)
	}
	note := p.Reason
	switch {
	case !p.Warmable:
		note += "; no warm-up sent"
	default:
		if err := c.warm(p.Target); err != nil {
			note += "; warm-up failed: " + err.Error()
		} else {
			note += "; warm-up sent"
		}
	}
	return nil, 0, fmt.Errorf("%w: %s", errColdEmbedder, note)
}
