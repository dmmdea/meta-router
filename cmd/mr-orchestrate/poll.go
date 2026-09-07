package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/copilotlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/profiles"
	"github.com/dmmdea/meta-router/internal/orch/quotapoll"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// pollState carries per-(lane,subject) last-poll stamps, rate-limiting the
// status-triggered polls (the explicit `poll` subcommand bypasses them). W2:
// keyed by "lane|subject"; pre-W2 single-account files carried LastClaude /
// LastCodex, migrated on load so a deployed machine's stamps survive.
type pollState struct {
	Last map[string]time.Time `json:"last"`
	// Legacy single-account stamps, migrated into Last on load. Pointers so a
	// nil (post-migration) is actually omitted — a zero time.Time struct is not
	// dropped by omitempty (review finding).
	LastClaude *time.Time `json:"last_claude,omitempty"`
	LastCodex  *time.Time `json:"last_codex,omitempty"`
	// Codex plan facts from the last SUCCESSFUL default-subject wham poll
	// (quotapoll.CodexFacts): plan type, whether a 5h window was reported,
	// whether a 5h window appeared in a sibling additional-limits block of
	// the same response, the models the plan lists, the models carrying
	// their own limits. EVIDENCE for status/probe planning — and, since
	// v0.40.6, the ONE admission-adjacent reader: codex5hEstimateGate reads
	// CodexHas5h + CodexSaw5hElsewhere + CodexFactsAt (freshness) when the
	// operator arms codex_5h_estimate_off. An outage preserves the last
	// facts (a failed fetch is not "no plan").
	CodexPlan           string          `json:"codex_plan,omitempty"`
	CodexHas5h          *bool           `json:"codex_has_5h,omitempty"`
	CodexSaw5hElsewhere *bool           `json:"codex_saw_5h_elsewhere,omitempty"`
	CodexUndecodable5h  *bool           `json:"codex_undecodable_5h,omitempty"`
	CodexModels         map[string]bool `json:"codex_models,omitempty"`
	CodexAdditional     []string        `json:"codex_additional_limits,omitempty"`
	CodexFactsAt        *time.Time      `json:"codex_facts_at,omitempty"`
}

func stampKey(lane, subject string) string {
	if subject == "" {
		subject = "default"
	}
	return lane + "|" + subject
}

func loadPollState() pollState {
	var ps pollState
	if b, err := os.ReadFile(statepaths.PollState()); err == nil {
		_ = json.Unmarshal(b, &ps)
	}
	if ps.Last == nil {
		ps.Last = map[string]time.Time{}
	}
	if ps.LastClaude != nil && !ps.LastClaude.IsZero() { // migrate legacy stamps once
		if _, ok := ps.Last[stampKey("claude", "default")]; !ok {
			ps.Last[stampKey("claude", "default")] = *ps.LastClaude
		}
		ps.LastClaude = nil
	}
	if ps.LastCodex != nil && !ps.LastCodex.IsZero() {
		if _, ok := ps.Last[stampKey("codex", "default")]; !ok {
			ps.Last[stampKey("codex", "default")] = *ps.LastCodex
		}
		ps.LastCodex = nil
	}
	return ps
}

func savePollState(ps pollState) {
	b, err := json.Marshal(ps)
	if err != nil {
		return
	}
	tmp := statepaths.PollState() + fmt.Sprintf(".tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, statepaths.PollState())
}

func pollDue(last time.Time, minMin int, now time.Time) bool {
	if minMin <= 0 {
		minMin = 5
	}
	return last.IsZero() || now.Sub(last) >= time.Duration(minMin)*time.Minute
}

// subjectFetch is one profile's poll result.
type subjectFetch struct {
	Lane    string
	Subject string
	Origin  string // oauth_poll | wham_poll | copilot_poll
	OK      bool   // fetch reached the endpoint (typed window absences still count OK)
	Res     quotapoll.Result
	// CapMilli is a MEASURED monthly capacity the poll carried (copilot: the
	// plan's credit entitlement × 1000). 0 = the poll carries no capacity;
	// the ledger keeps whatever it has (a config estimate, or an older poll).
	CapMilli int64
	// CodexFacts rides along on a codex fetch (nil for every other lane).
	CodexFacts *quotapoll.CodexFacts
}

// pollFetch is the NETWORK half: config-gated, rate-limited HTTP over the
// profile registry, with NO ledger involvement. Runs OUTSIDE ledger.Update —
// two 20s timeouts under the write lock would exceed the 30s lock-steal and
// reintroduce the cross-process race (W1 review finding, carried into W2).
type pollFetch struct {
	subjects []subjectFetch
}

func fetchPolls(cfg orchcfg.Config, reg profiles.Registry, ps pollState, force bool, now time.Time) pollFetch {
	var f pollFetch
	poll := func(lane, origin string, gate bool, do func(cred string) (quotapoll.Result, *quotapoll.CodexFacts)) {
		if !gate {
			return
		}
		for _, p := range reg.Lane(lane) {
			if !p.Provisioned {
				continue // never poll a home without credentials
			}
			if !force && !pollDue(ps.Last[stampKey(lane, p.Subject)], cfg.PollMinIntervalMin, now) {
				continue
			}
			res, facts := do(p.CredPath(lane))
			sf := subjectFetch{Lane: lane, Subject: p.Subject, Origin: origin, OK: true, Res: res, CodexFacts: facts}
			for _, a := range res.Absences {
				if a.Window == "all" { // not_logged_in / refresh_failed / http_* / parse_error
					sf.OK = false
				}
			}
			f.subjects = append(f.subjects, sf)
		}
	}
	poll("claude", "oauth_poll", cfg.OAuthUsagePoll, func(cred string) (quotapoll.Result, *quotapoll.CodexFacts) {
		return quotapoll.PollClaudeAt(cred, now), nil
	})
	poll("codex", "wham_poll", cfg.CodexUsagePoll, func(cred string) (quotapoll.Result, *quotapoll.CodexFacts) {
		r, f := quotapoll.PollCodexFactsAt(cred, now)
		return r, &f
	})
	f.pollCopilot(cfg, ps, force, now)
	return f
}

// pollCopilot is the copilot lane's provider poll. It sits outside the
// profile registry on purpose: the lane has no CLI home to be "provisioned"
// — its credential is the gh keyring account named by copilot_token_user,
// minted per poll exactly as per dispatch (never an ambient env token, which
// could report and bill a different GitHub account). Same config gate,
// rate limit and typed-absence discipline as the other pollers.
func (f *pollFetch) pollCopilot(cfg orchcfg.Config, ps pollState, force bool, now time.Time) {
	if !cfg.CopilotUsagePoll || cfg.CopilotTokenUser == "" {
		return // unconfigured lane: nothing to poll, nothing to state
	}
	if !force && !pollDue(ps.Last[stampKey("copilot", "default")], cfg.PollMinIntervalMin, now) {
		return
	}
	sf := subjectFetch{Lane: "copilot", Subject: "default", Origin: "copilot_poll"}
	tok, err := copilotlane.MintToken(cfg.CopilotTokenUser)
	if err != nil {
		sf.Res.Absences = append(sf.Res.Absences, quotapoll.Absence{Lane: quotapoll.LaneCopilot, Window: "all", Reason: "not_logged_in"})
		f.subjects = append(f.subjects, sf)
		return
	}
	res, facts := quotapoll.PollCopilot(tok, now)
	sf.Res, sf.OK = res, true
	for _, a := range res.Absences {
		if a.Window == "all" {
			sf.OK = false
		}
	}
	// Only a CREDIT entitlement may set the credit cap: a legacy
	// premium-request plan reports requests, and 300 requests are not 300
	// credits. The unit check is what keeps the two regimes apart.
	if facts.Unit == "credits" && facts.Entitlement > 0 {
		sf.CapMilli = int64(facts.Entitlement * 1000)
	}
	f.subjects = append(f.subjects, sf)
}

// applyPolls lands fetched snapshots per subject through the provider path
// inside an ALREADY-OPEN Update closure (sub-second; no network).
func applyPolls(l *ledger.Ledger, f pollFetch, now time.Time) {
	for _, sf := range f.subjects {
		if sf.CapMilli > 0 {
			// A vendor-reported entitlement is a MEASURED capacity: it replaces
			// the config estimate (and clears the estimate marking, so the
			// admission gate may exhaust on it, S2R-3) for the month window.
			l.SetCapacity(sf.Lane, ledger.WinMonth, sf.CapMilli)
		}
		if _, note := quotasig.ApplySnapshotsSubject(l, sf.Subject, sf.Res.Snapshots, quotaTracePath(), sf.Origin, now); note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
	}
}

// finishPolls advances stamps and maintains the scoped-alert latch AFTER a
// committed ledger transaction. The latch is touched only on a SUCCESSFUL
// claude fetch of the DEFAULT subject (the scoped limit is an account fact of
// the primary session; a skipped/failed poll leaves the latch untouched — a
// transient not_logged_in must never wipe a live latch, W1 review finding).
func finishPolls(f pollFetch, ps *pollState, now time.Time) {
	for _, sf := range f.subjects {
		ps.Last[stampKey(sf.Lane, sf.Subject)] = now
		recordCodexFacts(ps, sf, now)
	}
	savePollState(*ps)
	for _, sf := range f.subjects {
		if sf.Lane == "claude" && (sf.Subject == "" || sf.Subject == "default") && sf.OK {
			writeScopedAlert(sf.Res.Scoped, now)
		}
	}
}

// combined flattens all subjects' results (surface for the poll command +
// status absences). Absences carry their subject via the typed field already.
func (f pollFetch) combined() quotapoll.Result {
	var all quotapoll.Result
	for _, sf := range f.subjects {
		all.Snapshots = append(all.Snapshots, sf.Res.Snapshots...)
		all.Absences = append(all.Absences, sf.Res.Absences...)
		all.Scoped = append(all.Scoped, sf.Res.Scoped...)
	}
	return all
}

// writeScopedAlert latches critical/warning scoped limits into
// scoped-alert.json; an empty set clears the latch (gated on a successful
// default-subject claude fetch by finishPolls).
func writeScopedAlert(scoped []quotapoll.ScopedAlert, now time.Time) {
	var crit []quotapoll.ScopedAlert
	for _, s := range scoped {
		if s.Severity == "critical" || s.Severity == "warning" {
			crit = append(crit, s)
		}
	}
	path := statepaths.ScopedAlert()
	if len(crit) == 0 {
		_ = os.Remove(path)
		return
	}
	out := struct {
		TS     time.Time               `json:"ts"`
		Alerts []quotapoll.ScopedAlert `json:"alerts"`
	}{now, crit}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := path + fmt.Sprintf(".tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// runPoll is the explicit `poll` subcommand: force every provisioned profile
// now, print the combined result JSON (absences included — typed).
func runPoll(args []string) error {
	fs := flag.NewFlagSet("poll", flag.ExitOnError)
	_ = fs.Parse(args)
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	reg, rerr := profiles.Load(profilesPath())
	if rerr != nil {
		// Fail-open like status/run: an operator typo must not silently stop a
		// scheduled poll — degrade to the default subject (review finding).
		fmt.Fprintln(os.Stderr, "warn: profiles registry invalid, default subject only:", rerr)
		reg = nil
	}
	ps := loadPollState()
	f := fetchPolls(cfg, reg, ps, true, now) // network OUTSIDE the ledger lock
	err := updateLedger(func(l *ledger.Ledger) {
		applyPolls(l, f, now)
	})
	if err != nil {
		return err
	}
	finishPolls(f, &ps, now)
	out, merr := json.MarshalIndent(f.combined(), "", "  ")
	if merr != nil {
		return merr
	}
	fmt.Println(string(out))
	return nil
}

// recordCodexFacts lands the plan facts of a SUCCESSFUL default-subject codex
// fetch in poll-state. A failed or non-default fetch leaves the previous facts
// untouched: a transient outage is not evidence that the plan changed, and a
// second profile's plan is not the primary account's.
func recordCodexFacts(ps *pollState, sf subjectFetch, now time.Time) {
	if sf.Lane != "codex" || !sf.OK || sf.CodexFacts == nil || (sf.Subject != "" && sf.Subject != "default") {
		return
	}
	f := sf.CodexFacts
	if f.PlanType == "" {
		return // a body without a plan_type carries no plan fact to record
	}
	ps.CodexPlan = f.PlanType
	has := f.Has5h
	ps.CodexHas5h = &has
	saw := f.Saw5hElsewhere()
	ps.CodexSaw5hElsewhere = &saw
	und := f.Undecodable5h
	ps.CodexUndecodable5h = &und
	ps.CodexModels = map[string]bool{}
	for m, u := range f.Models {
		ps.CodexModels[m] = u.Available
	}
	ps.CodexAdditional = nil
	for _, a := range f.Additional {
		ps.CodexAdditional = append(ps.CodexAdditional, a.Name)
	}
	t := now
	ps.CodexFactsAt = &t
}

// CodexPlanStatus is the status view of the recorded codex plan facts.
type CodexPlanStatus struct {
	PlanType         string          `json:"plan_type"`
	Has5hWindow      *bool           `json:"has_5h_window,omitempty"`
	Saw5hElsewhere   *bool           `json:"saw_5h_elsewhere,omitempty"`  // a sibling additional-limits block of the same response carried a 5h window (v0.40.6 corroboration)
	Undecodable5h    *bool           `json:"undecodable_5h,omitempty"`    // a short-window block was present but yielded no snapshot: unreadable, not absent
	Models           map[string]bool `json:"models,omitempty"`
	AdditionalLimits []string        `json:"additional_limits,omitempty"`
	ObservedAt       *time.Time      `json:"observed_at,omitempty"`
	// The codex_5h_estimate_off gate as it would decide RIGHT NOW (v0.40.6):
	// armed = the config knob; suppressing = all four legs hold; reason =
	// the leg that decided, in the gate's own words. Rendered from the same
	// function the dispatch path calls, so status and the receipt cannot
	// disagree.
	EstimateOffArmed       bool   `json:"estimate_off_armed"`
	EstimateOffSuppressing bool   `json:"estimate_off_suppressing"`
	EstimateOffReason      string `json:"estimate_off_reason"`
	Note                   string `json:"note"`
}

// codexPlanStatus renders poll-state's codex facts; nil when none were ever
// recorded (an absent block, not an empty one). cfg/now feed the gate view.
func codexPlanStatus(ps pollState, cfg orchcfg.Config, now time.Time) *CodexPlanStatus {
	// An ARMED knob always has a status surface, even with no facts at all:
	// otherwise the operator arms it, sees no change, runs status, and gets
	// no evidence the binary knows the knob exists — while the gate's own
	// "poll first" reason is exactly the diagnostic for that state.
	if ps.CodexPlan == "" && !cfg.Codex5hEstimateOff {
		return nil
	}
	gate := codex5hEstimateGate(cfg, ps, now)
	return &CodexPlanStatus{
		PlanType: ps.CodexPlan, Has5hWindow: ps.CodexHas5h, Saw5hElsewhere: ps.CodexSaw5hElsewhere,
		Undecodable5h: ps.CodexUndecodable5h, Models: ps.CodexModels,
		AdditionalLimits: ps.CodexAdditional, ObservedAt: ps.CodexFactsAt,
		EstimateOffArmed: cfg.Codex5hEstimateOff, EstimateOffSuppressing: gate.Suppress, EstimateOffReason: gate.Reason,
		Note: "evidence from the wham usage poll (default subject); admission does not read it — a 5h window omitted by wham was unreliable on Plus (Q10), so has_5h_window=false is an observation, not proof; the ONE admission-adjacent reader is codex_5h_estimate_off (default off), which suppresses the 5h capacity estimate only when has_5h_window=false is corroborated by saw_5h_elsewhere on fresh facts",
	}
}
