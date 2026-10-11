package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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
// a busy router. Cold path worst case: 100 + 150 ms, against the 950 ms wait it replaces. Both
// are clamped to the deadline (hookBudgets), so a small -timeout-ms cannot be overrun.
const (
	runningProbeCap = 100 * time.Millisecond
	warmUpCap       = 150 * time.Millisecond
)

// A router that is up but cannot get the embedder loaded (a bad model path, no memory, a seat
// that fails its health check) used to be loud: the embed call errored and the row said
// embedder-down. Skipping the wait removed that signal, so it now looks like an ordinary idle
// cold start, forever. A run of cold skips that outlasts any real load is therefore logged as
// cold-stuck-*. The run is read from the hook's own usage log: this many consecutive earlier
// cold rows, the oldest at least this old (a real load measured 2 to 8 seconds on the live
// router, so two minutes is not a slow load). Both numbers are chosen, not fitted. The count
// is per log, not per model: one index, one embedder, so they coincide; a log shared by hosts
// with different embedders would blur it, which is the named limit.
const (
	stuckMinPrior  = 3
	stuckMinSpan   = 120 * time.Second
	stuckTailBytes = 64 << 10
)

// errColdStuck marks a cold skip that is the latest of a run too long to be a load in progress.
var errColdStuck = errors.New("the warm-ups are not producing a loaded model")

// errColdEmbedder marks "skipped the embed wait because the model is not loaded". decide()
// matches it with errors.Is to log cold-skip-* instead of embedder-down: the embedder is not
// down, it is unloaded.
var errColdEmbedder = errors.New("embedder not loaded (cold): skipped the embed wait")

// hookBudgets splits the hard deadline. The embed call keeps everything but the 50 ms the
// surfacing needs AND the probe's ceiling, so probe + embed can never overrun the deadline
// (without that, a slow-but-ready /running would push a warm embed past it). With the cold
// skip off the arithmetic is exactly what it was.
func hookBudgets(timeoutMs int, coldSkip bool) (embedTO, probeTO, warmTO time.Duration) {
	embedTO = time.Duration(timeoutMs) * time.Millisecond
	if timeoutMs > 50 {
		embedTO = time.Duration(timeoutMs-50) * time.Millisecond
	}
	if !coldSkip {
		return embedTO, 0, 0
	}
	// The cold path runs the probe and then the warm-up and never the embed, so the two
	// together stay under half of what the embed call alone had (1/5 + 1/4 of it).
	probeTO = min(runningProbeCap, embedTO/5)
	warmTO = min(warmUpCap, embedTO/4)
	return embedTO - probeTO, probeTO, warmTO
}

// coldSkipRetriever wraps the primary ranker (embed, rerank or hybrid: all three need the
// embedder). probe and warm are fields so the decision is testable without a router.
type coldSkipRetriever struct {
	inner   scoredRetriever
	probe   func() retrievers.EmbedProbe
	warm    func(p retrievers.EmbedProbe) (string, error)
	warmTO  time.Duration                  // the warm-up's budget after clamping, named in the row
	history func() (n int, oldestTs int64) // the trailing run of cold rows in the usage log
	now     func() time.Time
}

func newColdSkip(inner scoredRetriever, endpointSpec, model string, probeTO, warmTO time.Duration, logPath string) *coldSkipRetriever {
	return &coldSkipRetriever{
		inner:  inner,
		warmTO: warmTO,
		probe:  func() retrievers.EmbedProbe { return retrievers.ProbeEmbedder(endpointSpec, model, probeTO) },
		warm: func(p retrievers.EmbedProbe) (string, error) {
			return retrievers.WarmUpEmbedder(p.Target, model, p.ListedAs, warmTO)
		},
		history: func() (int, int64) { return coldRun(logPath) },
		now:     time.Now,
	}
}

// coldRun reads the tail of the usage log and returns how many consecutive cold rows end it
// and the ts_unix of the oldest of them. Rows that never reached the embedder (a notification,
// a too-short prompt, an index refusal, a deadline) are skipped; any row that did, embedded or
// failed, ends the run. A missing or unreadable log is no history.
func coldRun(path string) (n int, oldestTs int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	off := max(fi.Size()-stuckTailBytes, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return 0, 0
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 {
		lines = lines[1:] // the window may start mid-line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var r struct {
			Mode string `json:"mode"`
			Ts   int64  `json:"ts_unix"`
		}
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &r) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(r.Mode, "cold-"):
			n, oldestTs = n+1, r.Ts
		case r.Mode == "" || r.Mode == "too-short" || r.Mode == "machine-turn" || r.Mode == "tpl-mismatch" || r.Mode == "error":
			continue
		default:
			return n, oldestTs
		}
	}
	return n, oldestTs
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
			note += fmt.Sprintf("; warm-up sent, load not confirmed within %v", c.warmTO)
		default:
			note += "; warm-up sent, load confirmed (" + state + ")"
		}
	}
	err := fmt.Errorf("%w: %s", errColdEmbedder, note)
	if n, oldest := c.history(); n >= stuckMinPrior {
		if span := c.now().Sub(time.Unix(oldest, 0)); span >= stuckMinSpan {
			err = fmt.Errorf("%w [stuck: %d consecutive cold-skip rows over %s: %w]", err, n+1, span.Round(time.Second), errColdStuck)
		}
	}
	return nil, 0, err
}
