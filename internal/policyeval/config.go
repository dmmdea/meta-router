package policyeval

import (
	"fmt"
	"strings"
)

// EffortUnrecorded marks evidence produced before effort was captured, and any
// rank-table entry naming no effort. It is a REAL config value, never a
// wildcard: such evidence says nothing about how a pinned effort performs, so
// it must not satisfy an entry that names one (B6 — unknown cells are counted,
// never imputed).
const EffortUnrecorded = "unrecorded"

// NormalizeEffort is the ONE place blank effort becomes EffortUnrecorded. BOTH
// sides — oracle rows and rank-table/seed entries — must go through it, or a
// whole class of configs becomes permanently uncoverable: the seed carries
// entries with Effort:"" (the local lane has no effort dial), so normalizing
// only the oracle side leaves those keys ending "...|", matching nothing and
// failing ParseConfig.
//
// It also TRIMS, for the reason normalizePins exists in mr-goldreplay: a padded
// value that passes validation and is then recorded verbatim is a distinct
// string from every other row's, which is the mislabelling this whole plan
// exists to end.
func NormalizeEffort(e string) string {
	e = strings.TrimSpace(e)
	if e == "" {
		return EffortUnrecorded
	}
	return e
}

// Config is the unit of routing evidence: a lane alone pools observations
// across whatever model and effort produced them, which is how 204 claude rows
// recorded Sonnet's results under Opus's routing decisions. router.Entry
// already carries exactly these three fields.
//
// The zero Config is ABSTAIN (the old ""-lane sentinel): its Key matches no
// cell, so a policy that abstains scores UNKNOWN rather than a guessed rate.
type Config struct {
	Lane   string `json:"lane"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// Key is the cell identity. It round-trips through ParseConfig whenever all
// three fields are non-blank; NormalizeEffort is the ONLY defaulting rule in
// this package — a blank lane or model is left blank and stays an unknown
// cell, never imputed.
func (c Config) Key() string { return c.Lane + "|" + c.Model + "|" + c.Effort }

// IsLane reports whether this config runs on the named lane. Every
// `lane == "claude"` comparison in the evaluator goes through here: comparing
// against the KEY instead silently yields 0 for ClaudeFraction, which makes
// `NonInferior = RatioCILo >= 1-margin && ClaudeFraction < 1` unconditionally
// true and destroys the non-inferiority verdict without failing anything.
func (c Config) IsLane(l string) bool { return c.Lane == l }

// ParseConfig reads a `lane|model|effort` key back into a Config.
func ParseConfig(s string) (Config, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 {
		return Config{}, fmt.Errorf("config key %q: want lane|model|effort", s)
	}
	for i, p := range parts {
		if strings.TrimSpace(p) == "" {
			return Config{}, fmt.Errorf("config key %q: empty field %d", s, i)
		}
	}
	return Config{Lane: parts[0], Model: parts[1], Effort: parts[2]}, nil
}
