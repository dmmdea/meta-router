// Package admission gates dispatch on ledger state. Design rule (Niyama /
// design brief): relegation, never rejection — an exhausted lane returns a
// usable ResumeAt so the caller defers or re-lanes; work is never dropped.
// RS5: EVERY denial carries a ResumeAt, estimating conservatively when the
// exhausted bucket lacks a known reset (5h → now+5h; 7d → now+24h, re-check
// daily). Reset-boundary thundering herd: callers should jitter their retry
// around ResumeAt; full smoothing is slice-4 scheduler work.
package admission

import (
	"fmt"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

type State string

const (
	Open      State = "open"
	Throttled State = "throttled"
	Exhausted State = "exhausted"
)

type Thresholds struct{ ThrottlePct, ExhaustPct float64 }

type Decision struct {
	Admit    bool
	State    State
	Reason   string
	ResumeAt time.Time // set on every denial (RS5)
}

// estimateSourced reports whether a bucket's numbers are a config guess
// (shadow-derived percentage over an estimate-sourced capacity). S2R-3: such
// numbers may THROTTLE but never EXHAUST — a provider observation (Source
// "provider") always carries denial weight regardless of the cap's origin.
func estimateSourced(b ledger.Bucket) bool {
	return b.Source != "provider" && b.CapSource == ledger.CapSourceEstimate
}

// estimatedResume is the RS5 fallback when an exhausted bucket has no known
// reset moment.
func estimatedResume(w ledger.WindowKind, now time.Time) time.Time {
	if w == ledger.Win7d {
		return now.Add(24 * time.Hour)
	}
	return now.Add(5 * time.Hour)
}

// Decide grades the lane's DEFAULT credential subject (pre-W2 behavior,
// unchanged). Multi-account callers use DecideSubject — mixing subjects in
// one decision would let account A's exhaustion mask account B's headroom.
func Decide(bs []ledger.Bucket, lane string, now time.Time, th Thresholds) Decision {
	return DecideSubject(bs, lane, "", now, th)
}

func subjectOrDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// DecideSubject is Decide scoped to one credential subject (W2).
func DecideSubject(bs []ledger.Bucket, lane, subject string, now time.Time, th Thresholds) Decision {
	want := subjectOrDefault(subject)
	d := Decision{Admit: true, State: Open}
	estimated := false
	for _, b := range bs {
		if b.Lane != lane || subjectOrDefault(b.Subject) != want {
			continue
		}
		if !b.ResetsAt.IsZero() && now.After(b.ResetsAt) {
			// The window's reset moment has PASSED: its percentages are stale
			// history, not live pressure — they must never gate admission. The
			// ledger persists the roll on its next write (Bucket.roll).
			continue
		}
		switch {
		case b.UsedPct < 0:
			if d.State == Open && d.Reason == "" {
				d.Reason = "no signal (shadow floor unlearned)"
			}
		case b.UsedPct >= th.ExhaustPct && !estimateSourced(b):
			d.Admit = false
			d.State = Exhausted
			resume := b.ResetsAt
			est := resume.IsZero()
			if est {
				resume = estimatedResume(b.Window, now)
			}
			if d.ResumeAt.IsZero() || resume.Before(d.ResumeAt) {
				d.ResumeAt = resume
				estimated = est
				d.Reason = fmt.Sprintf("%s window at %.1f%%", b.Window, b.UsedPct)
			}
		case b.UsedPct >= th.ThrottlePct && d.State != Exhausted:
			d.State = Throttled
			d.Reason = fmt.Sprintf("%s window at %.1f%%", b.Window, b.UsedPct)
			if b.UsedPct >= th.ExhaustPct {
				// S2R-3: the percentage crossed the exhaust threshold but its
				// numbers are a config guess — deprioritize, never deny.
				d.Reason += " (estimate-sourced: throttle only — denial needs a real provider signal, S2R-3)"
			}
		}
	}
	if d.State == Exhausted && estimated {
		d.Reason += " (estimated resume: no provider reset known)"
	}
	return d
}
