package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/freelane"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

// BEHAVIOURAL tests of the free-provider dispatchers (W4). Like the GLM
// egress tests they assert on OUTPUT and exit codes, never on source, and use
// the dry-run path: it prints the same gate decisions, endpoint and
// effective_cwd the live path would act on, without a byte leaving.

type freeDispatcher func(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error)

var freeDispatchers = map[string]freeDispatcher{
	"groq": runGroqLane, "cloudflare": runCloudflareLane, "openrouter": runOpenrouterLane, "nim": runNimLane, "gemini": runGeminiLane,
}

var freeModels = map[string]string{
	"groq": "openai/gpt-oss-120b", "cloudflare": "@cf/openai/gpt-oss-120b",
	"openrouter": "meta-llama/llama-3.3-70b-instruct:free", "nim": "nvidia/nemotron-3-ultra-550b-a55b", "gemini": "gemini-3.8-flash",
}

func freeState(t *testing.T, cfg map[string]any) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func provision(t *testing.T, state, lane string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(state, "free"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freelane.TokenPath(state, lane), []byte("test-key-"+lane+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freeDryRun(t *testing.T, lane, cwd string, force bool, rf recFields) (map[string]any, int, error) {
	t.Helper()
	var out bytes.Buffer
	code, err := freeDispatchers[lane](&out, "hello", freeModels[lane], "", cwd, 30, false, force, "test", "free lane behaviour", rf, strategyFields{})
	if err != nil {
		return nil, code, err
	}
	var got map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("dispatcher output is not JSON (%v):\n%s", jerr, out.String())
	}
	return got, code, nil
}

// No credential file → a typed deferral naming the path to provision, for
// every lane. Nothing is forced past it (there is nothing to force).
func TestFreeLaneUnconfiguredIsTypedDeferral(t *testing.T) {
	state := freeState(t, nil)
	for lane := range freeDispatchers {
		if lane == "gemini" {
			continue // gated before the credential check; covered below
		}
		got, code, err := freeDryRun(t, lane, "", true, recFields{})
		if err != nil {
			t.Fatalf("%s: %v", lane, err)
		}
		if code != exitDeferred || got["unconfigured"] != true || got["deferred"] != true {
			t.Fatalf("%s: want unconfigured deferral exit %d, got %d %v", lane, exitDeferred, code, got)
		}
		if !strings.Contains(got["reason"].(string), freelane.TokenPath(state, lane)) {
			t.Fatalf("%s: reason must name the token path: %v", lane, got["reason"])
		}
	}
}

// Provisioned → dry-run reaches the endpoint, with the egress decision and the
// resolved directory on the output. The inherited, unallowlisted cwd must be
// substituted (the GLM guarantee, inherited verbatim).
func TestFreeLaneDryRunShowsEndpointAndEnforcesPromptOnly(t *testing.T) {
	state := freeState(t, map[string]any{"free_cloudflare_account_id": "acct-42"})
	wd, _ := os.Getwd()
	for lane := range freeDispatchers {
		if lane == "gemini" {
			continue
		}
		provision(t, state, lane)
		got, code, err := freeDryRun(t, lane, "", false, recFields{})
		if err != nil || code != 0 {
			t.Fatalf("%s: exit %d err %v out %v", lane, code, err, got)
		}
		spec, _ := freelane.Provider(lane)
		ep, _ := got["endpoint"].(string)
		if !strings.HasPrefix(ep, "https://") || !strings.HasSuffix(ep, "/chat/completions") {
			t.Fatalf("%s: endpoint %q", lane, ep)
		}
		if lane == "cloudflare" && !strings.Contains(ep, "/accounts/acct-42/") {
			t.Fatalf("cloudflare endpoint must embed the configured account: %s", ep)
		}
		if got["rpm"] != float64(spec.RPM) || got["daily_cap"] != float64(spec.DailyCap) {
			t.Fatalf("%s: limits on the dry-run must be the resolved priors: %v", lane, got)
		}
		eff, _ := got["effective_cwd"].(string)
		if eff == "" || strings.EqualFold(eff, wd) || strings.HasPrefix(strings.ToLower(eff), strings.ToLower(wd)) {
			t.Fatalf("%s: inherited unallowlisted cwd must be substituted, got %q", lane, eff)
		}
		if gate, _ := got["egress_gate"].(string); !strings.Contains(gate, "ENFORCED") {
			t.Fatalf("%s: egress gate must say enforced: %q", lane, gate)
		}
		if _, leaked := got["token"]; leaked || strings.Contains(strings.ToLower(string(freeJSON(got))), "test-key-") {
			t.Fatalf("%s: the token must never appear on the output: %v", lane, got)
		}
	}
}

// An explicitly requested non-allowlisted cwd is refused (exit 6), force-proof.
func TestFreeLaneRefusesExplicitNonAllowlistedCwd(t *testing.T) {
	state := freeState(t, map[string]any{"glm_allow_repos": []string{}})
	provision(t, state, "groq")
	other := t.TempDir()
	got, code, err := freeDryRun(t, "groq", other, true, recFields{})
	if err != nil || code != exitEgressDenied || got["egress_denied"] != true {
		t.Fatalf("exit %d err %v out %v", code, err, got)
	}
}

// An allowlisted cwd is used as-is (receipted), not degenerated into a temp dir.
func TestFreeLaneUsesAllowlistedCwd(t *testing.T) {
	repo := t.TempDir()
	state := freeState(t, map[string]any{"glm_allow_repos": []string{repo}})
	provision(t, state, "nim")
	got, code, err := freeDryRun(t, "nim", repo, false, recFields{})
	if err != nil || code != 0 {
		t.Fatalf("exit %d err %v out %v", code, err, got)
	}
	if eff, _ := got["effective_cwd"].(string); !strings.EqualFold(eff, repo) {
		t.Fatalf("allowlisted cwd must be used as-is: %q", eff)
	}
}

// OpenRouter refuses a paid model even with a token and even under --force —
// exit 1 (config error), before anything is metered or sent.
func TestOpenrouterRefusesPaidModel(t *testing.T) {
	state := freeState(t, nil)
	provision(t, state, "openrouter")
	var out bytes.Buffer
	_, err := runOpenrouterLane(&out, "hello", "meta-llama/llama-3.3-70b-instruct", "", "", 30, false, true, "test", "paid model", recFields{}, strategyFields{})
	if err == nil || !strings.Contains(err.Error(), ":free") || !strings.Contains(err.Error(), "R10") {
		t.Fatalf("a paid model must be a config error citing :free and R10, got %v\n%s", err, out.String())
	}
}

// Cloudflare without an account id is a config error naming the field.
func TestCloudflareNeedsAccountID(t *testing.T) {
	state := freeState(t, nil)
	provision(t, state, "cloudflare")
	var out bytes.Buffer
	_, err := runCloudflareLane(&out, "hello", "@cf/openai/gpt-oss-120b", "", "", 30, false, false, "test", "no account", recFields{}, strategyFields{})
	if err == nil || !strings.Contains(err.Error(), "free_cloudflare_account_id") {
		t.Fatalf("got %v", err)
	}
}

// Gemini: gated with an empty allowlist (force-proof), seated for an
// allowlisted class only, gated again for any other class.
func TestGeminiClassGate(t *testing.T) {
	state := freeState(t, nil)
	provision(t, state, "gemini")
	got, code, err := freeDryRun(t, "gemini", "", true, recFields{TaskClass: "mechanical-text"})
	if err != nil || code != exitDeferred || got["gated"] != true {
		t.Fatalf("empty allowlist must gate even under --force: %d %v %v", code, err, got)
	}
	if !strings.Contains(got["reason"].(string), "free_gemini_allow_classes") {
		t.Fatalf("reason must name the config field: %v", got["reason"])
	}
	freeState(t, map[string]any{"free_gemini_allow_classes": []string{"mechanical-text"}})
	state = os.Getenv("MR_ORCH_STATE")
	provision(t, state, "gemini")
	if got, code, err := freeDryRun(t, "gemini", "", false, recFields{TaskClass: "mechanical-text"}); err != nil || code != 0 || got["dry_run"] != true {
		t.Fatalf("allowlisted class must dispatch: %d %v %v", code, err, got)
	}
	if got, code, _ := freeDryRun(t, "gemini", "", true, recFields{TaskClass: "hard-repo"}); code != exitDeferred || got["gated"] != true {
		t.Fatalf("a class outside the allowlist must stay gated: %d %v", code, got)
	}
	if got, code, _ := freeDryRun(t, "gemini", "", true, recFields{}); code != exitDeferred || got["gated"] != true {
		t.Fatalf("no class at all must stay gated: %d %v", code, got)
	}
}

// Kill-switch and per-lane off are force-proof policy refusals.
func TestFreeLaneOffSwitches(t *testing.T) {
	state := freeState(t, map[string]any{"free_lanes_off": true})
	provision(t, state, "groq")
	got, code, err := freeDryRun(t, "groq", "", true, recFields{})
	if err != nil || code != exitDeferred || got["lane_off"] != true || !strings.Contains(got["reason"].(string), "free_lanes_off") {
		t.Fatalf("%d %v %v", code, err, got)
	}
	state = freeState(t, map[string]any{"free_providers": map[string]any{"nim": map[string]any{"off": true}}})
	provision(t, state, "nim")
	provision(t, state, "groq")
	if got, code, _ := freeDryRun(t, "nim", "", true, recFields{}); code != exitDeferred || got["lane_off"] != true {
		t.Fatalf("per-lane off: %d %v", code, got)
	}
	if got, code, _ := freeDryRun(t, "groq", "", false, recFields{}); code != 0 || got["dry_run"] != true {
		t.Fatalf("switching nim off must not touch groq: %d %v", code, got)
	}
}

// A deferral and a refusal each land a receipt (every decision is countable).
func TestFreeLaneRefusalsAreReceipted(t *testing.T) {
	state := freeState(t, nil)
	_, _, _ = freeDryRun(t, "groq", "", false, recFields{}) // unconfigured
	provision(t, state, "groq")
	_, _, _ = freeDryRun(t, "groq", t.TempDir(), false, recFields{}) // egress denied
	raw, err := os.ReadFile(filepath.Join(state, "dispatch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 receipts, got %d:\n%s", len(lines), raw)
	}
	if !strings.Contains(lines[0], `"admit_state":"unconfigured"`) || !strings.Contains(lines[1], `"admit_state":"egress_denied"`) || !strings.Contains(lines[1], `"egress_gate"`) {
		t.Fatalf("receipts:\n%s", raw)
	}
}

// --- ledger accounting ------------------------------------------------------

func newLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	return ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
}

func TestApplyFreeOutcomeRequestsDay(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	l := newLedger(t)
	spec, _ := freelane.Provider("openrouter")
	cfg := orchcfg.Defaults()
	applyFreeOutcome(l, spec, "m:free", freelane.Outcome{Class: "ok"}, cfg, now)
	b, ok := l.Bucket("openrouter", ledger.WinDay)
	if !ok || b.ShadowTokens != 1000 || b.CapTokens != 50_000 || b.CapSource != ledger.CapSourceEstimate || !b.ResetsAt.Equal(ledger.NextDailyReset(now)) {
		t.Fatalf("day bucket: %+v", b)
	}
	if b.UsedPct != 2 {
		t.Fatalf("1 of 50 requests = 2%%, got %v", b.UsedPct)
	}
	// A vendor 429 counts as a request AND (day-class: no retry-after) exhausts the day.
	applyFreeOutcome(l, spec, "m:free", freelane.Outcome{Class: "rate_limit"}, cfg, now)
	b, _ = l.Bucket("openrouter", ledger.WinDay)
	if b.ShadowTokens != 2000 || b.UsedPct != 100 || b.Source != "provider" || b.ProviderSource != ledger.ProviderSourceLimit || !b.ResetsAt.Equal(ledger.NextDailyReset(now)) {
		t.Fatalf("429 must exhaust the day until midnight: %+v", b)
	}
	// spawn_error meters nothing.
	l2 := newLedger(t)
	applyFreeOutcome(l2, spec, "m:free", freelane.Outcome{Class: "spawn_error"}, cfg, now)
	if b, _ := l2.Bucket("openrouter", ledger.WinDay); b.ShadowTokens != 0 {
		t.Fatalf("a transport failure never reached the vendor: %+v", b)
	}
}

func TestApplyFreeOutcomeMinuteClass429DoesNotExhaustTheDay(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	l := newLedger(t)
	spec, _ := freelane.Provider("groq")
	o := freelane.Outcome{Class: "rate_limit", Rate: freelane.RateInfo{RetryAfter: 7 * time.Second}}
	applyFreeOutcome(l, spec, "openai/gpt-oss-120b", o, orchcfg.Defaults(), now)
	b, _ := l.Bucket("groq", ledger.WinDay)
	if b.Source == "provider" || b.UsedPct >= 95 {
		t.Fatalf("a per-minute 429 must not exhaust the day window: %+v", b)
	}
	if b.ShadowTokens != 1000 {
		t.Fatalf("it still counts as a request: %+v", b)
	}
	// A day-class 429 with a long retry-after does exhaust, until that horizon.
	o = freelane.Outcome{Class: "rate_limit", Rate: freelane.RateInfo{RetryAfter: 3 * time.Hour}}
	applyFreeOutcome(l, spec, "openai/gpt-oss-120b", o, orchcfg.Defaults(), now)
	b, _ = l.Bucket("groq", ledger.WinDay)
	if b.UsedPct != 100 || !b.ResetsAt.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("day-class 429: %+v", b)
	}
}

func TestApplyFreeOutcomeGroqHeadersAreProviderTruth(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	l := newLedger(t)
	spec, _ := freelane.Provider("groq")
	o := freelane.Outcome{Class: "ok", Rate: freelane.RateInfo{Present: true, LimitRequests: 1000, RemainingRequests: 600, ResetRequests: 90 * time.Minute}}
	applyFreeOutcome(l, spec, "openai/gpt-oss-120b", o, orchcfg.Defaults(), now)
	b, _ := l.Bucket("groq", ledger.WinDay)
	if b.Source != "provider" || b.ProviderSource != ledger.ProviderSourcePoll || b.UsedPct != 40 || !b.ResetsAt.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("headers must land as a poll-authority observation: %+v", b)
	}
	if b.CapTokens != 1_000_000 || b.CapSource != "" {
		t.Fatalf("the header limit is the MEASURED cap (estimate mark cleared): %+v", b)
	}
}

func TestApplyFreeOutcomeCloudflareNeurons(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	l := newLedger(t)
	spec, _ := freelane.Provider("cloudflare")
	cfg := orchcfg.Defaults()
	o := freelane.Outcome{Class: "ok", Usage: freelane.Usage{Prompt: 1000, Completion: 1000}}
	applyFreeOutcome(l, spec, "@cf/openai/gpt-oss-120b", o, cfg, now)
	b, _ := l.Bucket("cloudflare", ledger.WinDay)
	// 1000*31818/1e6 + 1000*68182/1e6 = 31.818 + 68.182 = 100 neurons exactly.
	if b.ShadowTokens != 100_000 || b.CapTokens != 10_000_000 || b.UsedPct != 1 {
		t.Fatalf("neurons: %+v", b)
	}
	// Unknown model: floor of 1 neuron, never zero.
	applyFreeOutcome(l, spec, "@cf/nobody/knows", o, cfg, now)
	if b, _ = l.Bucket("cloudflare", ledger.WinDay); b.ShadowTokens != 101_000 {
		t.Fatalf("unknown model must meter the floor: %+v", b)
	}
}

func TestApplyFreeOutcomeNIMTrialAnd402(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	l := newLedger(t)
	spec, _ := freelane.Provider("nim")
	cfg := orchcfg.Defaults()
	applyFreeOutcome(l, spec, "nvidia/nemotron-3-ultra-550b-a55b", freelane.Outcome{Class: "ok"}, cfg, now)
	b, ok := l.Bucket("nim", ledger.WinTrial)
	if !ok || b.ShadowTokens != 1000 || b.CapTokens != 1_000_000 || b.UsedPct != 0.1 {
		t.Fatalf("trial credits: %+v", b)
	}
	if _, day := l.Bucket("nim", ledger.WinDay); day {
		t.Fatal("nim must never get a day window")
	}
	// A month later the pool has NOT rolled.
	later := now.AddDate(0, 1, 0)
	applyFreeOutcome(l, spec, "nvidia/nemotron-3-ultra-550b-a55b", freelane.Outcome{Class: "ok"}, cfg, later)
	if b, _ = l.Bucket("nim", ledger.WinTrial); b.ShadowTokens != 2000 {
		t.Fatalf("trial pool must accumulate, never roll: %+v", b)
	}
	// 402 = credits gone: exhausted, re-check in 24h.
	applyFreeOutcome(l, spec, "nvidia/nemotron-3-ultra-550b-a55b", freelane.Outcome{Class: "payment_required"}, cfg, later)
	b, _ = l.Bucket("nim", ledger.WinTrial)
	if b.UsedPct != 100 || b.ProviderSource != ledger.ProviderSourceLimit || !b.ResetsAt.Equal(later.Add(24*time.Hour)) {
		t.Fatalf("402 must latch the pool exhausted for a day: %+v", b)
	}
	// After the re-check horizon the next dispatch keeps the LIFETIME count
	// (3 credits spent + this one), instead of restarting the pool at zero.
	recheck := later.Add(25 * time.Hour)
	applyFreeOutcome(l, spec, "nvidia/nemotron-3-ultra-550b-a55b", freelane.Outcome{Class: "ok"}, cfg, recheck)
	b, _ = l.Bucket("nim", ledger.WinTrial)
	if b.ShadowTokens != 4000 || b.UsedPct != 0.4 || b.Source != "shadow" {
		t.Fatalf("the re-check roll must preserve the pool's history: %+v", b)
	}
}

// --- route states -----------------------------------------------------------

func TestLaneStatesCarryFreeLanes(t *testing.T) {
	state := freeState(t, map[string]any{"free_providers": map[string]any{"openrouter": map[string]any{"off": true}}})
	provision(t, state, "groq")
	provision(t, state, "gemini")
	now := time.Now().UTC()
	ls := laneStates(nil, fuses.Seed(), orchcfg.Load(configPath()), now)
	want := map[string]string{"groq": "open", "cloudflare": "unconfigured", "openrouter": "off", "nim": "unconfigured", "gemini": "gated"}
	for lane, st := range want {
		if ls[lane].State != st {
			t.Fatalf("%s = %q, want %q (all: %v)", lane, ls[lane].State, st, ls)
		}
	}
	// Exhausted day window → exhausted, with the reset as resume.
	reset := ledger.NextDailyReset(now)
	snap := []ledger.Bucket{{Lane: "groq", Window: ledger.WinDay, UsedPct: 100, Source: "provider", ResetsAt: reset}}
	ls = laneStates(snap, fuses.Seed(), orchcfg.Load(configPath()), now)
	if ls["groq"].State != "exhausted" || !ls["groq"].ResumeAt.Equal(reset) {
		t.Fatalf("groq: %+v", ls["groq"])
	}
	// Kill-switch masks all five as off.
	freeState(t, map[string]any{"free_lanes_off": true})
	ls = laneStates(nil, fuses.Seed(), orchcfg.Load(configPath()), now)
	for _, lane := range freelane.Lanes {
		if ls[lane].State != "off" {
			t.Fatalf("%s must be off under the kill-switch: %+v", lane, ls[lane])
		}
	}
}

// Route-level gemini refinement: a non-empty allowlist seats the lane only for
// the classes it names; every free lane is masked for the router until then.
func TestRouteGeminiClassRefinementAndFreeLanesMasked(t *testing.T) {
	state := freeState(t, map[string]any{"free_gemini_allow_classes": []string{"mechanical-text"}})
	provision(t, state, "gemini")
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	d := buildRouteDecision(cfg, fuses.Seed(), nil, "mechanical-text", 0, now, spendDownReq{})
	if d.QuotaState["gemini"] != "open" {
		t.Fatalf("allowlisted class must show gemini open: %v", d.QuotaState)
	}
	d = buildRouteDecision(cfg, fuses.Seed(), nil, "hard-repo", 0, now, spendDownReq{})
	if d.QuotaState["gemini"] != "gated" {
		t.Fatalf("non-allowlisted class must gate gemini: %v", d.QuotaState)
	}
	for _, lane := range []string{"groq", "cloudflare", "openrouter", "nim"} {
		if d.QuotaState[lane] != "unconfigured" {
			t.Fatalf("%s: %v", lane, d.QuotaState)
		}
	}
	// No seed row names a free lane (R14a: rows are earned by the gold probe).
	if freelane.IsLane(d.Lane) {
		t.Fatalf("the seed must not route to a free lane before it is measured: %+v", d)
	}
}

// --exclude free expands to the five lanes.
func TestParseExcludeFreeGroup(t *testing.T) {
	got, err := parseExclude([]string{"free", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "claude,cloudflare,gemini,groq,nim,openrouter" {
		t.Fatalf("got %v", got)
	}
	if got, _ := parseExclude([]string{"nim"}); strings.Join(got, ",") != "nim" {
		t.Fatalf("a single free lane stays single: %v", got)
	}
}

// The run switch reaches every free lane (and refuses --extra there).
func TestRunSwitchReachesFreeLanes(t *testing.T) {
	state := freeState(t, map[string]any{"free_cloudflare_account_id": "acct-1", "free_gemini_allow_classes": []string{"mechanical-text"}})
	for lane, model := range freeModels {
		provision(t, state, lane)
		var out bytes.Buffer
		code, err := doRun(runOpts{Prompt: "hello", Lane: lane, Model: model, Class: "mechanical-text", Live: false, Origin: "test", Desc: "switch"}, &out)
		if err != nil || code != 0 || !strings.Contains(out.String(), `"dry_run": true`) {
			t.Fatalf("%s: code %d err %v\n%s", lane, code, err, out.String())
		}
		out.Reset()
		if _, err := doRun(runOpts{Prompt: "hello", Lane: lane, Model: model, Extra: "--verbose", Origin: "test"}, &out); err == nil || !strings.Contains(err.Error(), "--extra") {
			t.Fatalf("%s: --extra must be refused on an HTTP lane: %v", lane, err)
		}
	}
}

func freeJSON(v any) []byte { b, _ := json.Marshal(v); return b }
