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
// The two ceilings are CHOSEN, not fitted. The router answers /running in about a millisecond
// on loopback, so 100 ms is a bound a healthy router never approaches and a wedged one cannot
// spend the deadline with. The warm-up holds its request open only until the router lists the
// model as loading (6 ms on the live router it was measured against); 150 ms is a ceiling for
// a busy router. Cold path worst case: 100 + 150 ms, against the 950 ms wait it replaces.
const (
	runningProbeCap = 100 * time.Millisecond
	warmUpCap       = 150 * time.Millisecond
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
	warm  func(p retrievers.EmbedProbe) (string, error)
}

func newColdSkip(inner scoredRetriever, endpointSpec, model string, probeTO time.Duration) *coldSkipRetriever {
	return &coldSkipRetriever{
		inner: inner,
		probe: func() retrievers.EmbedProbe { return retrievers.ProbeEmbedder(endpointSpec, model, probeTO) },
		warm: func(p retrievers.EmbedProbe) (string, error) {
			return retrievers.WarmUpEmbedder(p.Target, model, p.ListedAs, warmUpCap)
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
		switch state, err := c.warm(p); {
		case err != nil:
			note += "; warm-up failed: " + err.Error()
		case state == "":
			note += fmt.Sprintf("; warm-up sent, load not confirmed within %v", warmUpCap)
		default:
			note += "; warm-up sent, load confirmed (" + state + ")"
		}
	}
	return nil, 0, fmt.Errorf("%w: %s", errColdEmbedder, note)
}
