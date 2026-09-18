package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/jevclient"
	"github.com/dmmdea/meta-router/internal/orch/freelane"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	vp "github.com/dmmdea/meta-router/internal/orch/strategy/verifierpilot"
)

// The Jev column is an EXTERNAL, calibrated reference against the same corpus
// the local column runs: a second reading of the same 40 snippets by a model
// that answers with probabilities instead of a logprob margin. It exists so the
// local ceiling has something to be a ceiling BELOW — a local decisive accuracy
// is uninterpretable on its own at this n.
//
// It is off by default and it spends: $0.000701 for the committed 40-snippet
// corpus, measured on the live run of 2026-09-18 and read out of the responses
// (about $0.0000175 per snippet — the per-call figure the backlog estimated
// from, which is why the whole run costs four times the estimate that was
// carried over as a run total). It is never part of a test run and never part
// of the default command.

// jevVerdictKey is the local key the verdict question is filed under. Question
// keys are never sent to the model — the whole question lives in the
// instructions and criteria — so this string is ours alone.
const jevVerdictKey = "verdict"

// jevCriteria are the three options the reference column may answer with. They
// restate the same gate the local column is given, in the closed form a choice
// question needs, and the third option is what lets the model decline: a choice
// picks the nearest option unless "cannot tell" is on the list.
var jevCriteria = map[string]string{
	"yes":    "The code is correct: no bugs, does what its name and signature imply",
	"no":     "The code has a bug or does not do what its name and signature imply",
	"unsure": "Cannot tell from this snippet alone",
}

// jevRunner is the reference column's state for one run: the configured client,
// the one question every snippet is asked, and the metering we print at the end
// so the run's real cost is read from the response rather than estimated.
type jevRunner struct {
	client    jevclient.Client
	questions map[string]jevclient.Question
	// servedModel is the DATED build the vendor actually answered with. A moved
	// build invalidates every threshold fitted on the old one, so the column
	// prints it rather than the id we asked for.
	servedModel string
	cost        float64
	calls       int
}

// newJevRunner loads the credential and builds the column's client. The
// credential comes from the orchestrator's own credential file — the same path
// the free lanes read, never the environment — and a missing file is a TYPED
// absence the caller turns into a warning, never a failure: the column is an
// optional extra on a measurement run that must still produce the local column.
func newJevRunner(question, endpoint string) (*jevRunner, error) {
	key, err := freelane.LoadToken(statepaths.StateDir(), "openrouter")
	if err != nil {
		return nil, err
	}
	return &jevRunner{
		client: jevclient.Client{Key: key, Endpoint: endpoint},
		questions: map[string]jevclient.Question{
			jevVerdictKey: {
				Type:         "choice",
				Instructions: question,
				Criteria:     jevCriteria,
			},
		},
	}, nil
}

// evaluate runs one snippet through the reference column and returns the record
// in the same shape the local column produces, so one ceiling computation
// serves both.
//
// Confidence is the PROBABILITY OF THE CHOSEN OPTION — a real probability from
// a distribution that sums to 1. It is deliberately NOT the response's own
// `confidence` field: that field is not max(probabilities) (measured here:
// {0.60, 0.40} answered 0.40, {0.67, 0.33} answered 0.51), its v1 formula is
// unpublished, and the selective-risk curves this column feeds need a score
// whose meaning is fixed.
func (j *jevRunner) evaluate(ctx context.Context, s vp.Snippet) vp.Record {
	t0 := time.Now()
	resp, err := j.client.Evaluate(ctx, map[string]string{"snippet": s.Snippet}, j.questions)
	rec := vp.Record{
		Snippet:   idOr(s),
		Label:     s.Label,
		LatencyMS: time.Since(t0).Milliseconds(),
	}
	if err != nil {
		// Transport, HTTP or decode failure: an errored record, exactly like the
		// local column's spawn/parse error. It is a non-answer, it is counted as
		// a loss whenever accepted, and it never becomes a false pass.
		rec.Verdict = vp.VerdictErrored
		rec.Reason = err.Error()
		return rec
	}
	j.calls++
	j.cost += resp.Usage.Cost
	j.servedModel = resp.Model
	rec.Model = resp.Model

	a := resp.Answers[jevVerdictKey]
	choice := strings.ToLower(strings.TrimSpace(a.Choice))
	switch choice {
	case "yes":
		rec.Verdict = vp.VerdictPass
		rec.Confidence = a.Probabilities[a.Choice]
	case "no":
		rec.Verdict = vp.VerdictFail
		rec.Confidence = a.Probabilities[a.Choice]
	case "unsure":
		// An honest decline. Confidence stays 0: a non-answer sorts to the
		// bottom of coverage, the same convention the local column's defer uses.
		rec.Verdict = vp.VerdictDefer
	default:
		// A 200 that named no option, or an option we never offered. Neither is
		// a verdict, so it is an error rather than a quiet defer.
		rec.Verdict = vp.VerdictErrored
		rec.Reason = fmt.Sprintf("answer carried no known option (choice=%q)", a.Choice)
	}
	rec.Agree = vp.Agreement(s.Label, rec.Verdict)
	return rec
}

// meter renders what the column actually spent, read from the responses rather
// than estimated. The served build is named because it is the thing every
// number in the block is conditional on.
func (j *jevRunner) meter() string {
	model := j.servedModel
	if model == "" {
		model = "(no call was answered)"
	}
	return fmt.Sprintf("  served_model=%s  answered_calls=%d  cost=$%.6f", model, j.calls, j.cost)
}
