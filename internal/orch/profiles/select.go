package profiles

// SubjectState is a subject's routing-time health, derived by the caller from
// per-subject admission + binding pace slack.
type SubjectState struct {
	State string   // "open" | "throttled" | "exhausted" | "hard_stop" | ...
	Slack *float64 // binding pace slack; nil = unknown
}

// stateRank orders admission states best→worst for selection.
func stateRank(s string) int {
	switch s {
	case "open":
		return 0
	case "throttled":
		return 1
	default: // exhausted / hard_stop / unknown — least preferred
		return 2
	}
}

// Select returns the credential subject that should carry this lane's
// dispatch, and a non-empty provenance reason WHEN it rotated off the
// registry-first subject (typed-limit-only rotation). Order:
//  1. only PROVISIONED subjects are eligible (an un-provisioned home has no
//     credentials — never selectable; relegation upstream handles the case
//     where every subject is exhausted);
//  2. best admission state (open > throttled > exhausted);
//  3. among equal states, higher binding slack — but ONLY when BOTH slacks
//     are known (a one-sided measurement never overrides operator preference);
//  4. registry order (R15: account-1 first unless configured otherwise).
// Pure: no I/O, no network (Bible B2).
func Select(reg Registry, lane string, states map[string]SubjectState) (subject string, why string) {
	ps := reg.Lane(lane)
	var firstEligible string
	best := ""
	var bestSt SubjectState
	for _, p := range ps {
		if !p.Provisioned {
			continue
		}
		st := states[p.Subject]
		if firstEligible == "" {
			firstEligible = p.Subject
		}
		if best == "" {
			best, bestSt = p.Subject, st
			continue
		}
		if better(st, bestSt) {
			best, bestSt = p.Subject, st
		}
	}
	if best == "" {
		// No provisioned subject — caller falls back to the registry-first
		// subject's default home (single-account machines never hit this: the
		// default subject is provisioned by definition when logged in).
		return firstEligibleOr(ps), ""
	}
	if best != firstEligible {
		why = "rotated to " + best + ": " + rotationReason(bestSt, states[firstEligible])
	}
	return best, why
}

// better reports whether candidate c should beat the incumbent b.
func better(c, b SubjectState) bool {
	rc, rb := stateRank(c.State), stateRank(b.State)
	if rc != rb {
		return rc < rb
	}
	if c.Slack != nil && b.Slack != nil && *c.Slack != *b.Slack {
		return *c.Slack > *b.Slack
	}
	return false // equal state, no decisive slack → keep incumbent (registry order)
}

func rotationReason(chosen, firstSt SubjectState) string {
	switch stateRank(firstSt.State) {
	case 2:
		return "registry-first subject " + firstSt.State + " (typed limit)"
	case 1:
		return "registry-first subject throttled"
	default:
		return "higher pace headroom"
	}
}

func firstEligibleOr(ps []Profile) string {
	if len(ps) > 0 {
		return ps[0].Subject
	}
	return "default"
}
