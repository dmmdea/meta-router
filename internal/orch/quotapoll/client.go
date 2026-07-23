// Package quotapoll polls vendor usage endpoints for MEASURED window facts
// (reconciliation W1 / frontier Tier-A #1-2). Absence is typed and stated,
// never inferred. Pollers run only from cmd-layer trigger points (status /
// poll) — never the route hot path (Bible B2).
package quotapoll

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// guardedClient: fixed timeout, refuses every redirect (a vendor endpoint
// that suddenly redirects is a fault or an attack — either way we stop).
func guardedClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("quotapoll: redirects refused")
		},
	}
}

// getJSON fetches url with a bearer token. https is required except for
// loopback (httptest). Body capped at 2MiB. The token is never logged (R10).
func getJSON(c *http.Client, rawURL, bearer string, extra map[string]string) ([]byte, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, err
	}
	host := u.Hostname()
	loopback := host == "127.0.0.1" || host == "::1" || host == "localhost"
	if u.Scheme != "https" && !loopback {
		return nil, 0, fmt.Errorf("quotapoll: https required for %s", host)
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

func httpReason(code int) string { return "http_" + strconv.Itoa(code) }

// redactErr strips anything after "Bearer " if an error string ever embeds a
// header (defense in depth for logs; R10 tokens never surface).
func redactErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "Bearer "); i >= 0 {
		return s[:i] + "Bearer <redacted>"
	}
	return s
}
