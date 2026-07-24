package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

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
	Origin  string // oauth_poll | wham_poll
	OK      bool   // fetch reached the endpoint (typed window absences still count OK)
	Res     quotapoll.Result
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
	poll := func(lane, origin string, gate bool, do func(cred string) quotapoll.Result) {
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
			res := do(p.CredPath(lane))
			sf := subjectFetch{Lane: lane, Subject: p.Subject, Origin: origin, OK: true, Res: res}
			for _, a := range res.Absences {
				if a.Window == "all" { // not_logged_in / refresh_failed / http_* / parse_error
					sf.OK = false
				}
			}
			f.subjects = append(f.subjects, sf)
		}
	}
	poll("claude", "oauth_poll", cfg.OAuthUsagePoll, func(cred string) quotapoll.Result { return quotapoll.PollClaudeAt(cred, now) })
	poll("codex", "wham_poll", cfg.CodexUsagePoll, func(cred string) quotapoll.Result { return quotapoll.PollCodexAt(cred, now) })
	return f
}

// applyPolls lands fetched snapshots per subject through the provider path
// inside an ALREADY-OPEN Update closure (sub-second; no network).
func applyPolls(l *ledger.Ledger, f pollFetch, now time.Time) {
	for _, sf := range f.subjects {
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
	err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
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
