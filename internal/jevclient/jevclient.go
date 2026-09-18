// Package jevclient is a minimal client for TypeSafe's Jev decision model as
// served through OpenRouter's decisions path. Jev is a System One model: it
// does not generate text — it takes a state and a map of typed questions and
// returns typed answers with probabilities, in one parallel pass.
//
// Scope is deliberately one endpoint and one model. The operator's standing
// rule on this key is that its credits fund Jev only, so the served model is a
// package CONSTANT (Model) and there is no field, flag, option or environment
// variable through which a caller could name a different one. A caller that
// wants another model wants a different package.
//
// The credential is never taken from the environment: the caller loads it from
// the orchestrator's credential file (freelane.LoadToken(statepaths.StateDir(),
// "openrouter")) and passes it in Key — the same explicit-config rule the free
// lanes follow, because the operator's shell may carry keys for other
// businesses.
package jevclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Model is the pinned Jev build this client asks for. It is a constant, not a
// configurable field: see the package doc. The served build is dated and comes
// back in Response.Model — log it, because a moved build invalidates every
// threshold fitted on the old one.
const Model = "typesafe/jev-1.13"

// DefaultEndpoint is the one route that serves Jev. It lives here and nowhere
// else in this repo; callers override it only from a test (httptest).
const DefaultEndpoint = "https://openrouter.ai/api/alpha/decisions"

// DefaultTimeout bounds one call end to end. Measured round trips from this
// fleet are 340–1,140 ms; 20 s is a fault bound, not a budget.
const DefaultTimeout = 20 * time.Second

// maxBody caps what we read back. An answer set is kilobytes; a proxy's error
// page must not become a memory event (the freelane.maxBody rule, one size up
// because a per-candidate answer map can be large).
const maxBody = 16 << 20

// maxAttempts is the total number of tries for one call, retries included.
const maxAttempts = 3

// Question is one typed question. Type is "noul" (does this hold? → 0..1),
// "choice" (which one? → named options, probabilities sum to 1) or "score"
// (where on this described spectrum? → 2–10 ordered levels). The key a caller
// files a Question under is never sent to the model: the whole question lives
// in Instructions and Criteria.
//
// Criteria is any because the wire accepts a map (choice/noul) or an ordered
// array (score levels), and values may themselves be objects.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is one typed answer. Which fields are populated depends on the
// question type: Noul for a noul; Choice/Probabilities/Confidence for a
// choice; Score/Legend/Probabilities/Confidence for a score.
//
// Confidence is NOT max(Probabilities) and its formula is unpublished — do not
// build a gate on it. If a specific statistic is needed, compute it from
// Probabilities and call it something else.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Legend        []string           `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// Usage is the metered cost of one call. Output tokens are free on this model;
// Cost is in dollars and is what reconciles against the key's usage read-back.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// Response is the answered call. Model echoes the DATED build actually served
// (e.g. the pinned id plus a build date), which is a superset of what the
// vendor documents.
type Response struct {
	Model    string            `json:"model"`
	Answers  map[string]Answer `json:"answers"`
	Usage    Usage             `json:"usage"`
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
}

// Client is one configured caller. The zero value is unusable (Key is
// required); Endpoint, HTTP and Backoff all default.
type Client struct {
	// Key is the OpenRouter credential, loaded from the credential file by the
	// caller. It rides in the Authorization header and is NEVER serialized
	// into the body, a URL or an error string.
	Key string
	// Endpoint overrides DefaultEndpoint — TEST INJECTION ONLY (httptest).
	Endpoint string
	// HTTP overrides the default client (timeout DefaultTimeout, redirects
	// refused so a redirecting endpoint can never hand the bearer to another
	// host).
	HTTP *http.Client
	// Backoff is the first retry pause; it doubles per retry. 0 = 500ms.
	Backoff time.Duration
}

// requestBody is the wire shape. Model is written from the constant, so no
// call path exists that could put another id here.
type requestBody struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Evaluate asks every question about one state in a single call. Output tokens
// are free and an extra question adds a few tokens and almost no latency, so
// batching speculative questions here and discarding them in code is cheaper
// than a second call.
//
// Errors: a transport failure, a non-2xx the retry set could not clear, or an
// undecodable body. Retried (up to maxAttempts total, doubling backoff): 429
// rate limit, and 502/524/529 upstream. NOT retried: 400 (malformed question
// or unknown model — the body names the field), 401, 402 (credits exhausted),
// 413 (over the context ceiling). A gate that flips on rerun is inside the
// model's run-to-run noise and must never be met with more retries.
func (c Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (Response, error) {
	if strings.TrimSpace(c.Key) == "" {
		return Response{}, errors.New("jevclient: key is required (load it from the orchestrator credential file, never from the environment)")
	}
	if len(questions) == 0 {
		return Response{}, errors.New("jevclient: at least one question is required")
	}
	body, err := json.Marshal(requestBody{Model: Model, State: state, Questions: questions})
	if err != nil {
		return Response{}, fmt.Errorf("jevclient: encoding request: %w", err)
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{
			Timeout: DefaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("jevclient: redirects refused")
			},
		}
	}
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		resp, raw, status, err := c.do(ctx, hc, endpoint, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(status) {
			return Response{}, err
		}
		_ = raw
	}
	return Response{}, fmt.Errorf("jevclient: giving up after %d attempts: %w", maxAttempts, lastErr)
}

// do performs one attempt. It returns the HTTP status alongside the error so
// the caller can decide retryability; status 0 means the request never got a
// response (transport failure), which is retried like an upstream fault.
func (c Client) do(ctx context.Context, hc *http.Client, endpoint string, body []byte) (Response, []byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, nil, -1, fmt.Errorf("jevclient: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Key)
	httpResp, err := hc.Do(req)
	if err != nil {
		return Response{}, nil, 0, fmt.Errorf("jevclient: transport: %s", c.redact(err.Error()))
	}
	defer httpResp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(httpResp.Body, maxBody))
	if rerr != nil {
		return Response{}, nil, httpResp.StatusCode, fmt.Errorf("jevclient: reading response (HTTP %d): %s", httpResp.StatusCode, c.redact(rerr.Error()))
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return Response{}, raw, httpResp.StatusCode, fmt.Errorf("jevclient: HTTP %d: %s", httpResp.StatusCode, c.redact(snippet(raw)))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		// A 200 we cannot decode is OUR side of the contract breaking, not a
		// transient fault: do not retry it, surface it.
		return Response{}, raw, -1, fmt.Errorf("jevclient: decoding answers: %v (body: %s)", err, snippet(raw))
	}
	if len(out.Answers) == 0 {
		return Response{}, raw, -1, fmt.Errorf("jevclient: response carried no answers (body: %s)", snippet(raw))
	}
	return out, raw, httpResp.StatusCode, nil
}

// retryable is the documented transient set. A negative status is a local
// fault (encoding, decoding) and is never retried; 0 is a transport failure,
// which is.
func retryable(status int) bool {
	switch status {
	case 0, http.StatusTooManyRequests, http.StatusBadGateway, 524, 529:
		return true
	}
	return false
}

// redact removes the key from any message headed for a log or a receipt.
func (c Client) redact(s string) string {
	if c.Key != "" {
		s = strings.ReplaceAll(s, c.Key, "<redacted>")
	}
	if i := strings.Index(s, "Bearer "); i >= 0 {
		s = s[:i] + "Bearer <redacted>"
	}
	return s
}

// snippet bounds an error body so a vendor HTML page never lands whole in a
// log line. Rune-safe.
func snippet(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(truncated)"
}
