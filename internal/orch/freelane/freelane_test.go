package freelane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", "free", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureHeaders parses a captured "HTTP/1.1 NNN …" + header lines file.
func fixtureHeaders(t *testing.T, name string) (int, http.Header) {
	t.Helper()
	raw := string(fixture(t, name))
	sc := bufio.NewScanner(strings.NewReader(raw))
	h := http.Header{}
	status := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "HTTP/") {
			var code int
			var proto string
			if _, err := fmtSscanf(line, &proto, &code); err == nil {
				status = code
			}
			continue
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			h.Add(strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]))
		}
	}
	return status, h
}

// --- registry ---------------------------------------------------------------

func TestRegistryIsTheExclusionList(t *testing.T) {
	want := []string{"groq", "cloudflare", "openrouter", "nim", "gemini"}
	if strings.Join(Lanes, ",") != strings.Join(want, ",") {
		t.Fatalf("Lanes = %v, want %v (the vetted five, in vetting order)", Lanes, want)
	}
	for _, excluded := range []string{"cerebras", "github-models", "together", "fireworks", "deepseek", "mistral", "zhipu", "sambanova"} {
		if IsLane(excluded) {
			t.Fatalf("%s must NOT be a lane (excluded by the 2026-07-23 / 2026-09-06 vetting)", excluded)
		}
	}
	for _, l := range Lanes {
		s, ok := Provider(l)
		if !ok || s.Lane != l || s.BaseURL == "" || s.RPM <= 0 || s.DailyCap <= 0 {
			t.Fatalf("incomplete spec for %s: %+v", l, s)
		}
		if !strings.HasPrefix(s.BaseURL, "https://") {
			t.Fatalf("%s base must be https: %s", l, s.BaseURL)
		}
	}
	if s, _ := Provider(LaneNIM); s.Window != WindowTrial || s.Unit != UnitCredits {
		t.Fatalf("nim is a depletable trial (NVIDIA ToS), got %+v", s)
	}
	if s, _ := Provider(LaneOpenRouter); s.ModelSuffix != ":free" {
		t.Fatalf("openrouter must require :free models: %+v", s)
	}
	if s, _ := Provider(LaneGemini); !s.ClassGated {
		t.Fatal("gemini must be class-gated (free-tier data trains Google)")
	}
	if s, _ := Provider(LaneCloudflare); !s.NeedsAccountID || s.Unit != UnitNeurons {
		t.Fatalf("cloudflare needs an account id and meters neurons: %+v", s)
	}
	if s, _ := Provider(LaneGroq); !s.HeaderLimits {
		t.Fatal("groq reports x-ratelimit-* headers (documented 2026-09-06)")
	}
}

func TestResolveOverlay(t *testing.T) {
	base, _ := Provider(LaneGroq)
	s, ok := Resolve(LaneGroq, Limits{})
	if !ok || s != base {
		t.Fatalf("zero overlay must keep the prior: %+v vs %+v", s, base)
	}
	s, _ = Resolve(LaneGroq, Limits{RPM: 5, DailyCap: 100})
	if s.RPM != 5 || s.DailyCap != 100 {
		t.Fatalf("overlay must win: %+v", s)
	}
	if _, ok := Resolve("cerebras", Limits{RPM: 1}); ok {
		t.Fatal("an overlay cannot conjure an unlisted provider")
	}
}

// --- credential files -------------------------------------------------------

func TestLoadTokenAbsentIsTypedUnconfigured(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadToken(dir, LaneGroq)
	if err == nil || !isUnconfigured(err) {
		t.Fatalf("absent file must be ErrUnconfigured, got %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "free", "groq.token")) {
		t.Fatalf("the error must name the path to provision: %v", err)
	}
}

func TestLoadTokenTrimsAndRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "free"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := TokenPath(dir, LaneNIM)
	if err := os.WriteFile(p, []byte("nvapi-SECRET\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadToken(dir, LaneNIM)
	if err != nil || tok != "nvapi-SECRET" {
		t.Fatalf("got %q %v", tok, err)
	}
	if err := os.WriteFile(p, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(dir, LaneNIM); err == nil || isUnconfigured(err) || !strings.Contains(err.Error(), p) {
		t.Fatalf("an empty file is a config error naming the path, not unconfigured: %v", err)
	}
}

func isUnconfigured(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrUnconfigured.Error())
}

// --- request ----------------------------------------------------------------

func TestBuildBodyValidation(t *testing.T) {
	ok := RunReq{Lane: LaneGroq, Model: "openai/gpt-oss-120b", Prompt: "hi", Token: "k"}
	b, err := BuildBody(ok)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "openai/gpt-oss-120b" || got["max_tokens"] != float64(DefaultMaxTokens) || got["stream"] != false {
		t.Fatalf("body: %s", b)
	}
	if _, has := got["reasoning_effort"]; has {
		t.Fatalf("no effort pinned → no reasoning_effort: %s", b)
	}
	if strings.Contains(string(b), "k\"") && strings.Contains(string(b), "token") {
		t.Fatalf("token must never be serialized: %s", b)
	}
	for name, bad := range map[string]RunReq{
		"unknown lane":      {Lane: "cerebras", Model: "x", Prompt: "hi", Token: "k"},
		"no model":          {Lane: LaneGroq, Prompt: "hi", Token: "k"},
		"no prompt":         {Lane: LaneGroq, Model: "m", Token: "k"},
		"no token":          {Lane: LaneGroq, Model: "m", Prompt: "hi"},
		"openrouter paid":   {Lane: LaneOpenRouter, Model: "meta-llama/llama-3.3-70b-instruct", Prompt: "hi", Token: "k"},
		"openrouter suffix": {Lane: LaneOpenRouter, Model: "x:free-ish", Prompt: "hi", Token: "k"},
	} {
		if _, err := BuildBody(bad); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	if _, err := BuildBody(RunReq{Lane: LaneOpenRouter, Model: "meta-llama/llama-3.3-70b-instruct", Prompt: "hi", Token: "k"}); err == nil || !strings.Contains(err.Error(), "R10") {
		t.Fatalf("the :free refusal must cite R10 (zero spend): %v", err)
	}
	if _, err := BuildBody(RunReq{Lane: LaneOpenRouter, Model: "meta-llama/llama-3.3-70b-instruct:free", Prompt: "hi", Token: "k"}); err != nil {
		t.Fatalf(":free model must pass: %v", err)
	}
}

func TestBuildBodyEffortRule(t *testing.T) {
	for effort, want := range map[string]bool{"": false, EffortUnrecorded: false, "high": true, " low ": true} {
		b, err := BuildBody(RunReq{Lane: LaneNIM, Model: "m", Prompt: "p", Token: "k", Effort: effort, MaxTokens: 77})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		_, has := got["reasoning_effort"]
		if has != want {
			t.Fatalf("effort %q → reasoning_effort present=%v, want %v (%s)", effort, has, want, b)
		}
		if got["max_tokens"] != float64(77) {
			t.Fatalf("explicit max_tokens must be honoured: %s", b)
		}
	}
}

func TestEndpoint(t *testing.T) {
	cf, _ := Provider(LaneCloudflare)
	if _, err := Endpoint(cf, ""); err == nil || !strings.Contains(err.Error(), "free_cloudflare_account_id") {
		t.Fatalf("cloudflare without an account id must name the config field: %v", err)
	}
	u, err := Endpoint(cf, "abc123")
	if err != nil || u != "https://api.cloudflare.com/client/v4/accounts/abc123/ai/v1/chat/completions" {
		t.Fatalf("got %q %v", u, err)
	}
	g, _ := Provider(LaneGemini)
	if u, _ := Endpoint(g, ""); u != "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions" {
		t.Fatalf("gemini endpoint: %s", u)
	}
}

// --- parse ------------------------------------------------------------------

func TestParseLiveNIMCaptureIsNotAnAnswer(t *testing.T) {
	// The live 2026-09-06 capture: max_tokens 32, finish_reason "length",
	// content == reasoning_content. That is thinking, not output.
	status, h := fixtureHeaders(t, "nim-chat-200.headers")
	o := Parse(status, h, fixture(t, "nim-chat-200.json"))
	if o.Class != "empty_result" {
		t.Fatalf("reasoning echoed as content must be empty_result, got %s: %q", o.Class, o.Result)
	}
	if o.Model != "nvidia/nemotron-3-ultra-550b-a55b" || o.Usage.Prompt != 25 || o.Usage.Completion != 32 || o.FinishReason != "length" {
		t.Fatalf("attribution must survive: %+v", o)
	}
	if o.Rate.Present {
		t.Fatal("NIM sends no x-ratelimit headers (live-verified); Rate must be absent, not zero")
	}
}

func TestParseGroqSuccessCarriesRateHeaders(t *testing.T) {
	status, h := fixtureHeaders(t, "groq-chat-200.headers")
	o := Parse(status, h, fixture(t, "groq-chat-200.json"))
	if o.Class != "ok" || o.Result != "LANE PROBE OK" || o.Model != "openai/gpt-oss-120b" {
		t.Fatalf("%+v", o)
	}
	if !o.Rate.Present || o.Rate.LimitRequests != 1000 || o.Rate.RemainingRequests != 997 {
		t.Fatalf("rate headers: %+v", o.Rate)
	}
	if want := 2*time.Minute + 59*time.Second + 560*time.Millisecond; o.Rate.ResetRequests != want {
		t.Fatalf("reset = %s, want %s", o.Rate.ResetRequests, want)
	}
	if pct := o.Rate.UsedPct(); pct < 0.29 || pct > 0.31 {
		t.Fatalf("used pct = %v, want 0.3", pct)
	}
	if o.Rate.RetryAfter != 0 {
		t.Fatal("no retry-after on a 200")
	}
}

func TestParseGroq429(t *testing.T) {
	status, h := fixtureHeaders(t, "groq-429.headers")
	o := Parse(status, h, fixture(t, "groq-429.json"))
	if o.Class != "rate_limit" || !strings.Contains(o.Result, "RPD") {
		t.Fatalf("%+v", o)
	}
	if o.Rate.RetryAfter != 2*time.Second {
		t.Fatalf("retry-after header wins: %s", o.Rate.RetryAfter)
	}
	if o.Rate.UsedPct() != 100 {
		t.Fatalf("0 remaining of 1000 = 100%%, got %v", o.Rate.UsedPct())
	}
	// Without retry-after the reset duration is the fallback.
	h.Del("retry-after")
	o = Parse(status, h, fixture(t, "groq-429.json"))
	if want := time.Hour + 2*time.Minute + 3500*time.Millisecond; o.Rate.RetryAfter != want {
		t.Fatalf("fallback retry = %s, want %s", o.Rate.RetryAfter, want)
	}
}

func TestParseErrorClasses(t *testing.T) {
	cases := []struct {
		status int
		body   []byte
		class  string
		msg    string
	}{
		{429, fixture(t, "openrouter-429.json"), "rate_limit", "Rate limit exceeded"},
		{401, fixture(t, "auth-401.json"), "auth_error", "Invalid API Key"},
		{403, []byte(`{"error":{"message":"forbidden"}}`), "auth_error", "forbidden"},
		{402, []byte(`{"error":{"message":"You have exhausted your credits"}}`), "payment_required", "credits"},
		{413, []byte(`{"error":{"message":"Request too large for model"}}`), "too_large", "too large"},
		{500, []byte(`<html>bad gateway</html>`), "api_error", "bad gateway"},
		{503, nil, "api_error", "(empty body)"},
	}
	for _, c := range cases {
		o := Parse(c.status, http.Header{}, c.body)
		if o.Class != c.class || !strings.Contains(o.Result, c.msg) || o.HTTPStatus != c.status {
			t.Fatalf("status %d → %+v, want class %s containing %q", c.status, o, c.class, c.msg)
		}
	}
}

func TestParseSuccessShapes(t *testing.T) {
	if o := Parse(200, nil, []byte(`not json`)); o.Class != "parse_error" {
		t.Fatalf("%+v", o)
	}
	if o := Parse(200, nil, []byte(`{"model":"m","choices":[]}`)); o.Class != "empty_result" {
		t.Fatalf("%+v", o)
	}
	if o := Parse(200, nil, []byte(`{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"   "}}]}`)); o.Class != "empty_result" || !strings.Contains(o.Result, "stop") {
		t.Fatalf("%+v", o)
	}
	// Content that differs from the reasoning is a real answer.
	o := Parse(200, nil, []byte(`{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"42","reasoning_content":"thinking..."}}],"usage":{"prompt_tokens":3,"completion_tokens":9}}`))
	if o.Class != "ok" || o.Result != "42" || o.Usage.Completion != 9 {
		t.Fatalf("%+v", o)
	}
	// A long-body error is bounded.
	o = Parse(502, nil, []byte(strings.Repeat("x", 1000)))
	if len(o.Result) > 300 {
		t.Fatalf("error text must be bounded: %d", len(o.Result))
	}
}

// --- meter ------------------------------------------------------------------

func TestParseGroqDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"2m59.56s": 2*time.Minute + 59*time.Second + 560*time.Millisecond,
		"7.66s":    7*time.Second + 660*time.Millisecond,
		"1h2m":     time.Hour + 2*time.Minute,
		"1d":       24 * time.Hour,
		"45s":      45 * time.Second,
		" 3m ":     3 * time.Minute,
	}
	for in, want := range cases {
		got, ok := ParseGroqDuration(in)
		if !ok || got != want {
			t.Fatalf("%q → %s ok=%v, want %s", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "soon", "12", "1x"} {
		if _, ok := ParseGroqDuration(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

func TestNeuronsRoundsUp(t *testing.T) {
	rate := NeuronRate{In: 31818, Out: 68182} // gpt-oss-120b
	if n := Neurons(Usage{Prompt: 1_000_000, Completion: 1_000_000}, rate); n != 100000 {
		t.Fatalf("1M+1M tokens = %d, want 100000", n)
	}
	if n := Neurons(Usage{Prompt: 1, Completion: 0}, rate); n != 1 {
		t.Fatalf("a fractional neuron still counts: %d", n)
	}
	if n := Neurons(Usage{}, rate); n != 0 {
		t.Fatalf("no tokens, no neurons: %d", n)
	}
	if _, ok := DefaultNeuronRates()["@cf/openai/gpt-oss-120b"]; !ok {
		t.Fatal("the vetting's first cloudflare model must have a rate")
	}
}

// --- run ----------------------------------------------------------------------

func TestRunSendsBearerAndParses(t *testing.T) {
	var gotAuth, gotCT, gotReferer string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotReferer = r.Header.Get("HTTP-Referer")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("x-ratelimit-limit-requests", "50")
		w.Header().Set("x-ratelimit-remaining-requests", "49")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"meta-llama/llama-3.3-70b-instruct:free","choices":[{"finish_reason":"stop","message":{"content":"pong"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	o, raw, err := Run(context.Background(), RunReq{Lane: LaneOpenRouter, Model: "meta-llama/llama-3.3-70b-instruct:free", Prompt: "ping", Token: "sk-or-TEST", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if o.Class != "ok" || o.Result != "pong" || len(raw) == 0 {
		t.Fatalf("%+v %s", o, raw)
	}
	if gotAuth != "Bearer sk-or-TEST" || gotCT != "application/json" {
		t.Fatalf("auth=%q ct=%q", gotAuth, gotCT)
	}
	if gotReferer == "" {
		t.Fatal("openrouter attribution header expected")
	}
	if gotBody["model"] != "meta-llama/llama-3.3-70b-instruct:free" {
		t.Fatalf("body: %v", gotBody)
	}
	if !o.Rate.Present || o.Rate.RemainingRequests != 49 {
		t.Fatalf("rate: %+v", o.Rate)
	}
}

func TestRunRefusesPlaintextOffLoopback(t *testing.T) {
	o, _, err := Run(context.Background(), RunReq{Lane: LaneGroq, Model: "m", Prompt: "p", Token: "k", BaseURL: "http://api.groq.com/openai/v1"})
	if err == nil || o.Class != "config_error" || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plaintext to a real host must be refused before any byte leaves: %+v %v", o, err)
	}
}

func TestRunConfigErrorsNeverReachTheNetwork(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	for _, r := range []RunReq{
		{Lane: LaneOpenRouter, Model: "paid-model", Prompt: "p", Token: "k", BaseURL: srv.URL},
		{Lane: LaneGroq, Model: "m", Prompt: "p", BaseURL: srv.URL}, // no token
		{Lane: "cerebras", Model: "m", Prompt: "p", Token: "k", BaseURL: srv.URL},
	} {
		if o, _, err := Run(context.Background(), r); err == nil || o.Class != "config_error" {
			t.Fatalf("expected config_error for %+v, got %+v %v", r, o, err)
		}
	}
	if hits != 0 {
		t.Fatalf("config errors must not hit the network: %d requests", hits)
	}
}

func TestRunTransportFailureIsSpawnErrorRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	}))
	defer srv.Close()
	o, raw, err := Run(context.Background(), RunReq{Lane: LaneNIM, Model: "m", Prompt: "p", Token: "nvapi-SECRET-XYZ", BaseURL: srv.URL, TimeoutSec: 1})
	if err != nil || raw != nil {
		t.Fatalf("transport failure is an Outcome, not an error: %v %s", err, raw)
	}
	if o.Class != "spawn_error" || strings.Contains(o.Result, "SECRET") {
		t.Fatalf("%+v", o)
	}
}

func TestRunRefusesRedirects(t *testing.T) {
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target received the request (Authorization=%q)", r.Header.Get("Authorization"))
	}))
	defer leak.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leak.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	o, _, err := Run(context.Background(), RunReq{Lane: LaneGroq, Model: "m", Prompt: "p", Token: "k", BaseURL: srv.URL})
	if err != nil || o.Class != "spawn_error" || !strings.Contains(o.Result, "redirect") {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestRunUpstream429IsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("retry-after", "30")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()
	o, raw, err := Run(context.Background(), RunReq{Lane: LaneGemini, Model: "gemini-3.8-flash", Prompt: "p", Token: "k", BaseURL: srv.URL})
	if err != nil || o.Class != "rate_limit" || o.Rate.RetryAfter != 30*time.Second || len(raw) == 0 {
		t.Fatalf("%+v %v", o, err)
	}
}

// fmtSscanf isolates the status-line parse so the helper above stays readable.
func fmtSscanf(line string, proto *string, code *int) (int, error) {
	return fmt.Sscanf(line, "%s %d", proto, code)
}
