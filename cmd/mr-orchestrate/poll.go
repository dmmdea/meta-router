package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotapoll"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// pollState carries the last-poll stamps (rate limiting the status-triggered
// polls; the explicit `poll` subcommand bypasses the stamp).
type pollState struct {
	LastClaude time.Time `json:"last_claude"`
	LastCodex  time.Time `json:"last_codex"`
}

func loadPollState() pollState {
	var ps pollState
	if b, err := os.ReadFile(statepaths.PollState()); err == nil {
		_ = json.Unmarshal(b, &ps)
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

// pollFetch is the NETWORK half of polling: config-gated, rate-limited HTTP
// with NO ledger involvement. It must run OUTSIDE any ledger.Update — up to
// two 20s timeouts under the write lock would exceed the 30s lock-steal
// threshold and reintroduce the cross-process race Update exists to prevent
// (review finding, 2026-07-23).
type pollFetch struct {
	claudeRan, codexRan bool
	claudeOK            bool // fetch succeeded (typed window absences still count as success)
	claude, codex       quotapoll.Result
}

func fetchPolls(cfg orchcfg.Config, ps pollState, force bool, now time.Time) pollFetch {
	var f pollFetch
	if cfg.OAuthUsagePoll && (force || pollDue(ps.LastClaude, cfg.PollMinIntervalMin, now)) {
		f.claude = quotapoll.PollClaude(now)
		f.claudeRan = true
		f.claudeOK = true
		for _, a := range f.claude.Absences {
			if a.Window == "all" { // not_logged_in / refresh_failed / http_* / parse_error
				f.claudeOK = false
			}
		}
	}
	if cfg.CodexUsagePoll && (force || pollDue(ps.LastCodex, cfg.PollMinIntervalMin, now)) {
		f.codex = quotapoll.PollCodex(now)
		f.codexRan = true
	}
	return f
}

// applyPolls is the LEDGER half: land fetched snapshots through the provider
// path inside an ALREADY-OPEN Update closure (sub-second; no network).
func applyPolls(l *ledger.Ledger, f pollFetch, now time.Time) {
	if f.claudeRan {
		if _, note := quotasig.ApplySnapshots(l, f.claude.Snapshots, quotaTracePath(), "oauth_poll", now); note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
	}
	if f.codexRan {
		if _, note := quotasig.ApplySnapshots(l, f.codex.Snapshots, quotaTracePath(), "wham_poll", now); note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
	}
}

// finishPolls advances stamps and maintains the scoped-alert latch AFTER a
// successful ledger commit. The latch is touched ONLY on a SUCCESSFUL claude
// fetch: a successful poll with no critical/warning scoped limit clears it
// (vendor-refreshed truth); a skipped or failed poll leaves it untouched — a
// transient not_logged_in must never wipe a live latch (review finding).
func finishPolls(f pollFetch, ps *pollState, now time.Time) {
	if f.claudeRan {
		ps.LastClaude = now
	}
	if f.codexRan {
		ps.LastCodex = now
	}
	savePollState(*ps)
	if f.claudeRan && f.claudeOK {
		writeScopedAlert(f.claude.Scoped, now)
	}
}

func (f pollFetch) combined() quotapoll.Result {
	var all quotapoll.Result
	all.Snapshots = append(append(all.Snapshots, f.claude.Snapshots...), f.codex.Snapshots...)
	all.Absences = append(append(all.Absences, f.claude.Absences...), f.codex.Absences...)
	all.Scoped = append(all.Scoped, f.claude.Scoped...)
	return all
}

// writeScopedAlert latches critical/warning scoped limits into
// scoped-alert.json; an empty set clears the latch. Callers gate this on a
// successful claude fetch (finishPolls).
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

// runPoll is the explicit `poll` subcommand: force both polls now, print the
// combined result JSON (absences included — typed, never inferred).
func runPoll(args []string) error {
	fs := flag.NewFlagSet("poll", flag.ExitOnError)
	_ = fs.Parse(args)
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	ps := loadPollState()
	f := fetchPolls(cfg, ps, true, now) // network OUTSIDE the ledger lock
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
