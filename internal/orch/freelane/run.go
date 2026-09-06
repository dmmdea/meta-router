package freelane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeoutSec bounds one dispatch when the caller passes none.
const DefaultTimeoutSec = 120

// maxBody caps what we read back (a chat completion is kilobytes; a proxy's
// error page must not become a memory event).
const maxBody = 4 << 20

// Run performs one dispatch. Config failures (unknown lane, invalid model,
// missing account id, empty token) return an error AND a config_error
// Outcome; everything that reaches the network returns a classified Outcome
// with a nil error, so callers always have something to receipt.
func Run(ctx context.Context, r RunReq) (Outcome, []byte, error) {
	spec, ok := Provider(r.Lane)
	if !ok {
		err := fmt.Errorf("unknown free lane %q", r.Lane)
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	if r.BaseURL != "" {
		spec.BaseURL = r.BaseURL
		spec.NeedsAccountID = false
	}
	endpoint, err := Endpoint(spec, r.AccountID)
	if err != nil {
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	body, err := BuildBody(r)
	if err != nil {
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	host := u.Hostname()
	loopback := host == "127.0.0.1" || host == "::1" || host == "localhost"
	if u.Scheme != "https" && !loopback {
		// A key over plaintext is a leaked key. Same rule as quotapoll.getJSON.
		err := fmt.Errorf("free lane %s: https required for %s", r.Lane, host)
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	timeout := r.TimeoutSec
	if timeout <= 0 {
		timeout = DefaultTimeoutSec
	}
	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
		// A vendor endpoint that suddenly redirects is a fault or an attack;
		// following it could hand the bearer token to a third host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("freelane: redirects refused")
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	if r.Lane == LaneOpenRouter {
		// Documented optional attribution headers; harmless, and they keep the
		// orchestrator identifiable on the vendor side if a limit is disputed.
		req.Header.Set("HTTP-Referer", "https://github.com/dmmdea/meta-router")
		req.Header.Set("X-Title", "meta-router")
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport failure: DNS, TLS, timeout, refused redirect. OUR side of
		// the wire (or the network) — the breaker's spawn_error class, not a
		// vendor verdict. Redact the token in case a URL/header echo exists.
		return Outcome{Class: "spawn_error", Result: redact(err.Error(), r.Token)}, nil, nil
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if rerr != nil {
		return Outcome{Class: "spawn_error", HTTPStatus: resp.StatusCode, Result: "reading response: " + redact(rerr.Error(), r.Token)}, nil, nil
	}
	return Parse(resp.StatusCode, resp.Header, raw), raw, nil
}

// redact removes the token (and any Bearer tail) from a message destined for
// a receipt or log (R10: tokens never surface).
func redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "<redacted>")
	}
	if i := strings.Index(s, "Bearer "); i >= 0 {
		s = s[:i] + "Bearer <redacted>"
	}
	return s
}
