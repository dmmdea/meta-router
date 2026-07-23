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

// runPolls executes the config-gated usage polls inside an open ledger txn.
// force bypasses the rate-limit stamps (the `poll` subcommand). Returns the
// combined result for surfacing; the CRITICAL scoped-limit latch is written
// as a side effect (glm-alert passthrough pattern).
func runPolls(l *ledger.Ledger, cfg orchcfg.Config, ps *pollState, force bool, now time.Time) quotapoll.Result {
	var all quotapoll.Result
	if cfg.OAuthUsagePoll && (force || pollDue(ps.LastClaude, cfg.PollMinIntervalMin, now)) {
		res := quotapoll.PollClaude(now)
		if _, note := quotasig.ApplySnapshots(l, res.Snapshots, quotaTracePath(), "oauth_poll", now); note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
		all.Snapshots = append(all.Snapshots, res.Snapshots...)
		all.Absences = append(all.Absences, res.Absences...)
		all.Scoped = append(all.Scoped, res.Scoped...)
		ps.LastClaude = now
	}
	if cfg.CodexUsagePoll && (force || pollDue(ps.LastCodex, cfg.PollMinIntervalMin, now)) {
		res := quotapoll.PollCodex(now)
		if _, note := quotasig.ApplySnapshots(l, res.Snapshots, quotaTracePath(), "wham_poll", now); note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
		all.Snapshots = append(all.Snapshots, res.Snapshots...)
		all.Absences = append(all.Absences, res.Absences...)
		ps.LastCodex = now
	}
	writeScopedAlert(all.Scoped, now)
	return all
}

// writeScopedAlert latches CRITICAL scoped limits into scoped-alert.json so
// status surfaces them even between polls; a poll with no critical scoped
// limit clears the latch (the condition is vendor-refreshed, unlike the
// operator-ack'd GLM 1313 latch).
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
	var res quotapoll.Result
	err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		res = runPolls(l, cfg, &ps, true, now)
	})
	if err != nil {
		return err
	}
	savePollState(ps)
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
