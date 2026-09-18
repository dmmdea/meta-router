package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/jevclient"
	vp "github.com/dmmdea/meta-router/internal/orch/strategy/verifierpilot"
)

// answerBody renders one choice answer the way the vendor does.
func answerBody(choice string, probs map[string]float64) string {
	p, _ := json.Marshal(probs)
	return `{"model":"typesafe/jev-1.13-20260917","answers":{"` + jevVerdictKey +
		`":{"type":"choice","choice":"` + choice + `","probabilities":` + string(p) +
		`,"confidence":0.4}},"usage":{"input_tokens":493,"output_tokens":0,"cost":0.0000207},"id":"gen-1"}`
}

// newTestRunner builds a runner pointed at a fake endpoint with no credential
// file involved — the loader is exercised separately.
func newTestRunner(endpoint string) *jevRunner {
	return &jevRunner{
		client: jevclient.Client{Key: "k", Endpoint: endpoint},
		questions: map[string]jevclient.Question{
			jevVerdictKey: {Type: "choice", Instructions: defaultQuestion, Criteria: jevCriteria},
		},
	}
}

// Each option maps to the verdict the ceiling code understands, and the
// confidence carried forward is the PROBABILITY OF THE CHOSEN OPTION — never
// the response's own confidence field (0.4 in every fixture here, and never
// equal to the probability, which is the point).
func TestJevEvaluateMapsChoices(t *testing.T) {
	cases := []struct {
		name     string
		choice   string
		probs    map[string]float64
		label    vp.Label
		wantV    vp.Verdict
		wantConf float64
		wantGree bool
	}{
		{"yes on a good snippet", "yes", map[string]float64{"yes": 0.83, "no": 0.12, "unsure": 0.05}, vp.LabelGood, vp.VerdictPass, 0.83, true},
		{"choice echoed in another case still finds its probability", "Yes", map[string]float64{"yes": 0.77, "no": 0.20, "unsure": 0.03}, vp.LabelGood, vp.VerdictPass, 0.77, true},
		{"yes on a bad snippet", "yes", map[string]float64{"yes": 0.61, "no": 0.30, "unsure": 0.09}, vp.LabelBad, vp.VerdictPass, 0.61, false},
		{"no on a bad snippet", "no", map[string]float64{"yes": 0.09, "no": 0.88, "unsure": 0.03}, vp.LabelBad, vp.VerdictFail, 0.88, true},
		{"unsure is a non-answer", "unsure", map[string]float64{"yes": 0.4, "no": 0.2, "unsure": 0.4}, vp.LabelGood, vp.VerdictDefer, 0, false},
		{"an option we never offered", "maybe", map[string]float64{"maybe": 0.9}, vp.LabelGood, vp.VerdictErrored, 0, false},
		{"no option at all", "", nil, vp.LabelGood, vp.VerdictErrored, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, answerBody(c.choice, c.probs))
			}))
			defer srv.Close()

			j := newTestRunner(srv.URL)
			rec := j.evaluate(context.Background(), vp.Snippet{ID: "s1", Snippet: "func f() {}", Label: c.label})
			if rec.Verdict != c.wantV {
				t.Fatalf("verdict = %s, want %s", rec.Verdict, c.wantV)
			}
			if rec.Confidence != c.wantConf {
				t.Fatalf("confidence = %v, want %v (the probability of the chosen option, not the confidence field)", rec.Confidence, c.wantConf)
			}
			// a decoded 200 is a billed call even when it names no usable option: metered, never a free error
			if j.calls != 1 {
				t.Fatalf("calls = %d, want 1 (a decoded response is metered whatever it named)", j.calls)
			}
			if rec.Agree != c.wantGree {
				t.Fatalf("agree = %v, want %v", rec.Agree, c.wantGree)
			}
			if rec.Snippet != "s1" || rec.Label != c.label {
				t.Fatalf("record lost its identity: %+v", rec)
			}
			if rec.Model == "" {
				t.Fatalf("record did not record the served build: %+v", rec)
			}
		})
	}
}

// A call that never got an answer is an errored record — a non-answer that the
// ceiling counts as a loss whenever accepted — and it carries its reason. It is
// never a quiet defer and never a pass.
func TestJevEvaluateErroredCallIsANonAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"questions.verdict.criteria must be an object"}}`)
	}))
	defer srv.Close()

	j := newTestRunner(srv.URL)
	rec := j.evaluate(context.Background(), vp.Snippet{ID: "s1", Snippet: "x", Label: vp.LabelBad})
	if rec.Verdict != vp.VerdictErrored || rec.Agree {
		t.Fatalf("want an errored non-answer, got %+v", rec)
	}
	if !strings.Contains(rec.Reason, "criteria must be an object") {
		t.Fatalf("reason lost the vendor's message: %q", rec.Reason)
	}
	if j.calls != 0 || j.cost != 0 {
		t.Fatalf("an unanswered call was metered: calls=%d cost=%v", j.calls, j.cost)
	}
}

// The meter reads the run's cost out of the responses rather than estimating it,
// and names the dated build every number is conditional on.
func TestJevMeterReadsCostFromResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, answerBody("yes", map[string]float64{"yes": 0.8, "no": 0.2}))
	}))
	defer srv.Close()

	j := newTestRunner(srv.URL)
	for i := 0; i < 3; i++ {
		j.evaluate(context.Background(), vp.Snippet{ID: "s", Snippet: "x", Label: vp.LabelGood})
	}
	m := j.meter()
	if !strings.Contains(m, "answered_calls=3") || !strings.Contains(m, "cost=$0.000062") {
		t.Fatalf("meter = %q", m)
	}
	if !strings.Contains(m, jevclient.Model) {
		t.Fatalf("meter did not name the served build: %q", m)
	}
}

// writeCorpus drops a two-snippet corpus and returns its path.
func writeCorpus(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "corpus.jsonl")
	body := `{"id":"a","snippet":"func add(a, b int) int { return a + b }","label":"good"}` + "\n" +
		`{"id":"b","snippet":"func div(a, b int) int { return a / b }","label":"bad"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// FAIL OPEN: the column is armed but no credential file exists. The run warns,
// names the missing path, and still exits 0 — a measurement run must never be
// blocked by an optional extra.
func TestRunFailsOpenWithoutCredential(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)

	var out bytes.Buffer
	code := run(runConfig{
		corpus:    writeCorpus(t, t.TempDir()),
		outPath:   filepath.Join(t.TempDir(), "seed.jsonl"),
		question:  defaultQuestion,
		bin:       filepath.Join(state, "no-such-offload-binary"),
		jevColumn: true,
		jevOut:    filepath.Join(t.TempDir(), "seed-jev.jsonl"),
	}, &out)

	if code != 0 {
		t.Fatalf("exit %d, want 0 (fail open)", code)
	}
	s := out.String()
	if !strings.Contains(s, "WARNING: jev column skipped:") {
		t.Fatalf("no skip warning in output:\n%s", s)
	}
	if !strings.Contains(s, filepath.Join("free", "openrouter.token")) {
		t.Fatalf("the warning does not name the credential path to provision:\n%s", s)
	}
	if strings.Contains(s, "Jev reference column") {
		t.Fatalf("a skipped column still printed a block:\n%s", s)
	}
}

// The local column being unavailable does not cancel the reference column: the
// run warns about the binary, measures the column it can, and exits 0. No
// side-by-side table is printed, because there is nothing to compare against.
func TestRunReferenceColumnSurvivesAMissingLocalBinary(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "free"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "free", "openrouter.token"), []byte("test-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MR_ORCH_STATE", state)

	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Errorf("credential did not reach the request: %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, answerBody("no", map[string]float64{"yes": 0.11, "no": 0.86, "unsure": 0.03}))
	}))
	defer srv.Close()

	jevOut := filepath.Join(t.TempDir(), "seed-jev.jsonl")
	var out bytes.Buffer
	code := run(runConfig{
		corpus:      writeCorpus(t, t.TempDir()),
		outPath:     filepath.Join(t.TempDir(), "seed.jsonl"),
		question:    defaultQuestion,
		bin:         filepath.Join(state, "no-such-offload-binary"),
		jevColumn:   true,
		jevOut:      jevOut,
		jevEndpoint: srv.URL,
	}, &out)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if asked != 2 {
		t.Fatalf("%d calls for a 2-snippet corpus, want 2", asked)
	}
	s := out.String()
	for _, want := range []string{"WARNING: offload binary", "Jev reference column", "Wrote 2 records", "cost=$"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Verifier ceiling (PILOT") {
		t.Fatalf("a local block printed with no local binary:\n%s", s)
	}
	if strings.Contains(s, "decisive_accuracy         ") {
		t.Fatalf("a side-by-side table printed with only one column:\n%s", s)
	}

	recs, err := vp.Load(jevOut)
	if err != nil {
		t.Fatalf("seed load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("seed has %d records, want 2", len(recs))
	}
	// The bad snippet was called bad: an agreeing decisive record carrying a real
	// probability, which is what the selective-risk curves consume.
	if recs[1].Verdict != vp.VerdictFail || !recs[1].Agree || recs[1].Confidence != 0.86 {
		t.Fatalf("seed record wrong: %+v", recs[1])
	}
}

// Off by default: no flag, no credential read, no call, and the pre-existing
// fail-open behaviour on a missing binary is unchanged.
func TestRunWithoutTheColumnIsUnchanged(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)

	var out bytes.Buffer
	code := run(runConfig{
		corpus:   writeCorpus(t, t.TempDir()),
		outPath:  filepath.Join(t.TempDir(), "seed.jsonl"),
		question: defaultQuestion,
		bin:      filepath.Join(state, "no-such-offload-binary"),
	}, &out)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, "WARNING: offload binary") {
		t.Fatalf("output:\n%s", s)
	}
	if strings.Contains(s, "jev") || strings.Contains(s, "Jev") {
		t.Fatalf("the column spoke while disarmed:\n%s", s)
	}
}

// A corpus that cannot be read is the one fatal condition, column or no column.
func TestRunFailsOnAnUnreadableCorpus(t *testing.T) {
	var out bytes.Buffer
	if code := run(runConfig{corpus: filepath.Join(t.TempDir(), "absent.jsonl")}, &out); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}
