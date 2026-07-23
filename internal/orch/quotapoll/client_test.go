package quotapoll

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuardedClientRefusesRedirects(t *testing.T) {
	hops := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	_, code, err := getJSON(guardedClient(), srv.URL, "tok", nil)
	if err == nil && code == http.StatusOK {
		t.Fatal("redirect must not be followed")
	}
	if hops != 1 {
		t.Fatalf("exactly one hop allowed, got %d", hops)
	}
}

func TestGetJSONRefusesPlainHTTPNonLoopback(t *testing.T) {
	_, _, err := getJSON(guardedClient(), "http://internal.example/usage", "tok", nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain-http non-loopback must be refused, got %v", err)
	}
}

func TestGetJSONLimitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 3<<20)) // 3MiB
	}))
	defer srv.Close()
	b, _, err := getJSON(guardedClient(), srv.URL, "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 2<<20 {
		t.Fatalf("body must be capped at 2MiB, got %d", len(b))
	}
}

func TestRedactErrStripsBearer(t *testing.T) {
	got := redactErr(errors.New(`Get "x": header Authorization: Bearer sk-secret-token failed`))
	if strings.Contains(got, "sk-secret-token") {
		t.Fatalf("token leaked: %s", got)
	}
	if !strings.Contains(got, "Bearer <redacted>") {
		t.Fatalf("redaction marker missing: %s", got)
	}
}
