package main

import (
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/pace"
	"github.com/dmmdea/meta-router/internal/orch/profiles"
)

// selectedSubject is the W2 credential-subject choice for a dispatch, plus its
// isolated home and rotation provenance.
type selectedSubject struct {
	Subject        string
	Home           string // CLAUDE_CONFIG_DIR / CODEX_HOME; "" = the CLI's default
	RotationFrom   string // registry-first subject when rotated (else "")
	RotationReason string
}

// pickSubject chooses the credential subject that should carry a lane's
// dispatch, from the profile registry + per-subject admission/pace state.
// Single-profile lanes (every machine until a second account is provisioned)
// return the default subject with empty home/provenance — byte-identical to
// pre-W2. Pure over the passed snapshot; no network (B2).
func pickSubject(reg profiles.Registry, l *ledger.Ledger, lane string, now time.Time) selectedSubject {
	ps := reg.Lane(lane)
	if len(ps) <= 1 {
		return selectedSubject{} // default subject, default home
	}
	states := map[string]profiles.SubjectState{}
	for _, p := range ps {
		sb := l.SnapshotSubject(lane, p.Subject)
		d := admission.DecideSubject(sb, lane, p.Subject, now, defaultThresholds)
		st := profiles.SubjectState{State: string(d.State)}
		if s, ok := pace.Binding(sb, now); ok {
			st.Slack = &s
		}
		states[p.Subject] = st
	}
	subject, firstEligible, why := profiles.Select(reg, lane, states)
	sel := selectedSubject{Subject: subject}
	for _, p := range ps {
		if p.Subject == subject {
			sel.Home = p.Home
		}
	}
	if why != "" {
		sel.RotationFrom = firstEligible // the incumbent it rotated OFF, not blindly ps[0]
		sel.RotationReason = why
	}
	return sel
}
