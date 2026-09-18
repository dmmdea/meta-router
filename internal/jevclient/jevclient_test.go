package jevclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// choiceQuestion is the shape every test asks; the key is local and is never
// sent to the model.
func choiceQuestion() map[string]Question {
	return map[string]Question{
		"verdict": {
			Type:         "choice",
			Instructions: "Which option applies?",
			Criteria:     map[string]string{"yes": "It applies", "no": "It does not"},
		},
	}
}

const answeredBody = `{"model":"typesafe/jev-1.13-20260917",
  "answers":{"verdict":{"type":"choice","choice":"yes",
    "probabilities":{"yes":0.83,"no":0.17},"confidence":0.61}},
  "usage":{"input_tokens":493,"output_tokens":0,"cost":0.0000207},
  "id":"gen-abc","provider":"TypeSafe"}`

// The pinned build is written from the package constant on every call: a
// caller has no field, flag or env var through which another id could reach
// the wire. Severing the constant (or letting a caller override it) is red.
func TestRequestCarriesTheConstantModel(t *testing.T) {
	var gotModel, gotAuth string
	var gotState map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model     string              `json:"model"`
			State     map[string]any      `json:"state"`
			Questions map[string]Question `json:"questions"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("request body undecodable: %v", err)
		}
		gotModel, gotState, gotAuth = body.Model, body.State, r.Header.Get("Authorization")
		if _, ok := body.Questions["verdict"]; !ok {
			t.Errorf("questions map lost its key: %v", body.Questions)
		}
		io.WriteString(w, answeredBody)
	}))
	defer srv.Close()

	c := Client{Key: "test-key", Endpoint: srv.URL}
	if _, err := c.Evaluate(context.Background(), map[string]string{"snippet": "func f() {}"}, choiceQuestion()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if gotModel != Model {
		t.Fatalf("request named %q, want the package constant %q", gotModel, Model)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if gotState["snippet"] != "func f() {}" {
		t.Fatalf("state did not arrive verbatim: %v", gotState)
	}
}

// A body with no key is refused before anything leaves the machine — an empty
// credential would let the vendor answer 401 on a request already sent.
func TestEvaluateRefusesEmptyKeyAndEmptyQuestions(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		io.WriteString(w, answeredBody)
	}))
	defer srv.Close()

	if _, err := (Client{Endpoint: srv.URL}).Evaluate(context.Background(), "s", choiceQuestion()); err == nil {
		t.Fatal("empty key: want an error")
	}
	if _, err := (Client{Key: "k", Endpoint: srv.URL}).Evaluate(context.Background(), "s", nil); err == nil {
		t.Fatal("no questions: want an error")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("refused calls still reached the network %d times", n)
	}
}

// The answer map decodes with its probabilities intact — the probability of
// the chosen option is the only calibrated number in the payload.
func TestEvaluateDecodesAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, answeredBody)
	}))
	defer srv.Close()

	resp, err := (Client{Key: "k", Endpoint: srv.URL}).Evaluate(context.Background(), "s", choiceQuestion())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !strings.HasPrefix(resp.Model, Model) {
		t.Fatalf("served model %q does not start with the pinned id %q", resp.Model, Model)
	}
	a := resp.Answers["verdict"]
	if a.Choice != "yes" || a.Probabilities["yes"] != 0.83 {
		t.Fatalf("answer decoded wrong: %+v", a)
	}
	if resp.Usage.InputTokens != 493 || resp.Usage.Cost != 0.0000207 {
		t.Fatalf("usage decoded wrong: %+v", resp.Usage)
	}
	if resp.ID != "gen-abc" {
		t.Fatalf("generation id lost: %q", resp.ID)
	}
}

// A noul answer carries no confidence field at all; the decoder must not
// invent one.
func TestEvaluateDecodesNoul(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"typesafe/jev-1.13-20260917","answers":{"real":{"type":"noul","noul":0.94}},"usage":{"input_tokens":120,"output_tokens":0,"cost":0.000005}}`)
	}))
	defer srv.Close()

	resp, err := (Client{Key: "k", Endpoint: srv.URL}).Evaluate(context.Background(), "s",
		map[string]Question{"real": {Type: "noul", Instructions: "Does it hold?"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := resp.Answers["real"]; got.Noul != 0.94 || got.Confidence != 0 {
		t.Fatalf("noul answer decoded wrong: %+v", got)
	}
}

// 429 and the upstream faults are retried with backoff; the second try's
// answer is the result.
func TestEvaluateRetriesTransient(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, 524, 529} {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&hits, 1) == 1 {
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"busy"}}`)
				return
			}
			io.WriteString(w, answeredBody)
		}))
		c := Client{Key: "k", Endpoint: srv.URL, Backoff: time.Millisecond}
		resp, err := c.Evaluate(context.Background(), "s", choiceQuestion())
		if err != nil {
			t.Fatalf("status %d: Evaluate: %v", status, err)
		}
		if resp.Answers["verdict"].Choice != "yes" {
			t.Fatalf("status %d: retry lost the answer", status)
		}
		if n := atomic.LoadInt32(&hits); n != 2 {
			t.Fatalf("status %d: %d attempts, want 2", status, n)
		}
		srv.Close()
	}
}

// Retries are bounded: a permanently busy endpoint gives up, it does not hammer.
func TestEvaluateGivesUpAfterMaxAttempts(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := (Client{Key: "k", Endpoint: srv.URL, Backoff: time.Millisecond}).Evaluate(context.Background(), "s", choiceQuestion())
	if err == nil {
		t.Fatal("want an error after the retry budget")
	}
	if n := int(atomic.LoadInt32(&hits)); n != maxAttempts {
		t.Fatalf("%d attempts, want %d", n, maxAttempts)
	}
}

// The permanent statuses are surfaced immediately, body and all: a malformed
// question, an exhausted balance and an oversized state are facts to act on,
// not faults to wait out.
func TestEvaluateDoesNotRetryPermanent(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusBadRequest, `{"error":{"message":"questions.verdict.criteria must be an object"}}`, "criteria must be an object"},
		{http.StatusUnauthorized, `{"error":{"message":"No auth credentials found"}}`, "No auth credentials"},
		{http.StatusPaymentRequired, `{"error":{"message":"Insufficient credits"}}`, "Insufficient credits"},
		{http.StatusRequestEntityTooLarge, `{"error":{"message":"state too large"}}`, "state too large"},
	} {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		_, err := (Client{Key: "k", Endpoint: srv.URL, Backoff: time.Millisecond}).Evaluate(context.Background(), "s", choiceQuestion())
		if err == nil {
			t.Fatalf("status %d: want an error", tc.status)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: error %q does not carry the vendor's reason %q", tc.status, err, tc.want)
		}
		if n := atomic.LoadInt32(&hits); n != 1 {
			t.Fatalf("status %d: %d attempts, want exactly 1 (no retry)", tc.status, n)
		}
		srv.Close()
	}
}

// A 200 that is not the documented shape is a contract break, not a transient:
// surfaced once, never retried.
func TestEvaluateRejectsUndecodableAndEmpty(t *testing.T) {
	for _, body := range []string{`not json`, `{"model":"x","answers":{}}`} {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			io.WriteString(w, body)
		}))
		if _, err := (Client{Key: "k", Endpoint: srv.URL, Backoff: time.Millisecond}).Evaluate(context.Background(), "s", choiceQuestion()); err == nil {
			t.Fatalf("body %q: want an error", body)
		}
		if n := atomic.LoadInt32(&hits); n != 1 {
			t.Fatalf("body %q: %d attempts, want 1", body, n)
		}
		srv.Close()
	}
}

// The credential never survives into a message a log or a receipt could carry.
func TestErrorsRedactTheKey(t *testing.T) {
	const key = "sensitive-credential-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key `+key+` on Bearer sensitive-credential-value"}}`)
	}))
	defer srv.Close()

	_, err := (Client{Key: key, Endpoint: srv.URL}).Evaluate(context.Background(), "s", choiceQuestion())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the credential survived into the error: %q", err)
	}
}

// A cancelled context stops the wait between retries instead of sleeping it out.
func TestEvaluateHonoursContextDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	t0 := time.Now()
	if _, err := (Client{Key: "k", Endpoint: srv.URL, Backoff: 5 * time.Second}).Evaluate(ctx, "s", choiceQuestion()); err == nil {
		t.Fatal("want an error")
	}
	if el := time.Since(t0); el > 3*time.Second {
		t.Fatalf("waited %v — the backoff ignored the cancelled context", el)
	}
}

// The default route is the decisions path and nothing else: an empty Endpoint
// must not silently become some other URL.
func TestDefaultEndpointIsTheDecisionsPath(t *testing.T) {
	if !strings.HasSuffix(DefaultEndpoint, "/alpha/decisions") {
		t.Fatalf("DefaultEndpoint = %q — the only route that serves this model is the decisions path", DefaultEndpoint)
	}
	if !strings.HasPrefix(DefaultEndpoint, "https://") {
		t.Fatalf("DefaultEndpoint = %q — a credential over plaintext is a leaked credential", DefaultEndpoint)
	}
}
