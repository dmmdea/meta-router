package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/pace"
	"github.com/dmmdea/meta-router/internal/orch/profiles"
)

// runProfiles is the `profiles` subcommand: list the credential-profile
// registry with each subject's admission state, pace slack, provisioning
// status, and — for an unprovisioned profile — the exact login command the
// operator runs to provision it. Read-only (validation + display; meta-router
// never logs a subject in itself, R10).
func runProfiles(args []string) error {
	fs := flag.NewFlagSet("profiles", flag.ExitOnError)
	_ = fs.Parse(args)
	now := time.Now().UTC()
	reg, err := profiles.Load(profilesPath())
	if err != nil {
		return fmt.Errorf("profiles registry invalid: %w", err)
	}
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}

	type subjOut struct {
		Subject     string   `json:"subject"`
		Home        string   `json:"home,omitempty"`
		Provisioned bool     `json:"provisioned"`
		State       string   `json:"state,omitempty"`
		PaceSlack   *float64 `json:"pace_slack,omitempty"`
		LoginCmd    string   `json:"login_command,omitempty"` // present only when NOT provisioned
	}
	out := map[string][]subjOut{}
	for _, lane := range []string{"claude", "codex"} {
		for _, p := range reg.Lane(lane) {
			so := subjOut{Subject: p.Subject, Home: p.Home, Provisioned: p.Provisioned}
			if p.Provisioned {
				sb := l.SnapshotSubject(lane, p.Subject)
				so.State = string(admission.DecideSubject(sb, lane, p.Subject, now, defaultThresholds).State)
				if s, ok := pace.Binding(sb, now); ok {
					so.PaceSlack = &s
				}
			} else {
				so.LoginCmd = p.LoginCommand(lane)
			}
			out[lane] = append(out[lane], so)
		}
	}
	b, _ := json.MarshalIndent(map[string]any{
		"registry_path": profilesPath(),
		"lanes":         out,
		"note":          "meta-router USES operator-provisioned homes; it never logs a subject in. Provision an unprovisioned profile with its login_command, then re-run.",
	}, "", "  ")
	fmt.Println(string(b))
	return nil
}
