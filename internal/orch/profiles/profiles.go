// Package profiles is W2's credential-profile registry: each lane maps to an
// ORDERED list of subjects, each with an isolated auth home the operator
// provisioned by logging in once (CLAUDE_CONFIG_DIR=<home> claude /login,
// CODEX_HOME=<home> codex login — probe-verified 2026-07-23 that
// CLAUDE_CONFIG_DIR fully relocates the credential read on Windows).
// meta-router never creates, copies, or refreshes credentials (R10); it USES
// operator-provisioned homes. Registry order encodes operator preference
// (R15: account-2 before lane-downgrade for quality classes). An absent
// registry file means the implicit default: each lane's one "default"
// subject reading the CLI's own live home — byte-identical to pre-W2.
package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Profile is one credential subject for a lane.
type Profile struct {
	Subject string `json:"subject"`
	// Home is the isolated auth home; "" = the CLI's own default location
	// (the live ~/.claude / ~/.codex — used in place, never copied).
	Home string `json:"home"`
	// Provisioned reports whether the home currently holds the lane's
	// credential file. Derived at Load, never persisted: an unprovisioned
	// profile is LISTED with this mark, never silently skipped.
	Provisioned bool `json:"-"`
}

// Registry maps lane → ordered profiles.
type Registry map[string][]Profile

// credFile names each lane's credential artifact inside a home.
func credFile(lane string) string {
	switch lane {
	case "claude":
		return ".credentials.json"
	case "codex":
		return "auth.json"
	}
	return ""
}

// defaultRegistry is the implicit single-subject world.
func defaultRegistry() Registry {
	return Registry{
		"claude": {{Subject: "default", Home: ""}},
		"codex":  {{Subject: "default", Home: ""}},
	}
}

// Lane returns the lane's ordered profiles (implicit default for lanes the
// registry file omits — a partial file must not delete a lane's identity).
func (r Registry) Lane(lane string) []Profile {
	if ps, ok := r[lane]; ok && len(ps) > 0 {
		return ps
	}
	return defaultRegistry()[lane]
}

// MultiSubject reports whether any lane has more than one profile — the
// gate for the multi-subject status/selection surfaces (single-subject
// machines stay byte-identical to pre-W2).
func (r Registry) MultiSubject() bool {
	for _, lane := range []string{"claude", "codex"} {
		if len(r.Lane(lane)) > 1 {
			return true
		}
	}
	return false
}

// Load reads and validates the registry. Missing file → implicit default.
// Validation errors are LOUD (a typo'd home must not silently vanish a
// subject): duplicate subjects and nonexistent home dirs fail the load.
// Provisioning (credential file present) is a MARK, not an error — the
// operator sees it in `profiles` output with the exact login command.
func Load(path string) (Registry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			reg := defaultRegistry()
			markProvisioned(reg)
			return reg, nil
		}
		return nil, err
	}
	var reg Registry
	if err := json.Unmarshal(b, &reg); err != nil {
		return nil, fmt.Errorf("profiles.json: %w", err)
	}
	for lane, ps := range reg {
		seen := map[string]bool{}
		for i, p := range ps {
			if p.Subject == "" {
				return nil, fmt.Errorf("profiles.json: %s[%d] has an empty subject", lane, i)
			}
			if seen[p.Subject] {
				return nil, fmt.Errorf("profiles.json: %s has duplicate subject %q", lane, p.Subject)
			}
			seen[p.Subject] = true
			if p.Home != "" {
				if st, err := os.Stat(p.Home); err != nil || !st.IsDir() {
					return nil, fmt.Errorf("profiles.json: %s/%s home %q is not an existing directory", lane, p.Subject, p.Home)
				}
			}
		}
	}
	markProvisioned(reg)
	return reg, nil
}

func markProvisioned(reg Registry) {
	for lane, ps := range reg {
		cf := credFile(lane)
		for i := range ps {
			ps[i].Provisioned = provisioned(lane, ps[i].Home, cf)
		}
		reg[lane] = ps
	}
}

func provisioned(lane, home, credName string) bool {
	if credName == "" {
		return false
	}
	dir := home
	if dir == "" {
		ud, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		switch lane {
		case "claude":
			dir = filepath.Join(ud, ".claude")
		case "codex":
			dir = filepath.Join(ud, ".codex")
		}
	}
	_, err := os.Stat(filepath.Join(dir, credName))
	return err == nil
}

// CredPath returns the credential file the profile's poller should read.
func (p Profile) CredPath(lane string) string {
	cf := credFile(lane)
	if cf == "" {
		return ""
	}
	dir := p.Home
	if dir == "" {
		ud, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		switch lane {
		case "claude":
			dir = filepath.Join(ud, ".claude")
		case "codex":
			dir = filepath.Join(ud, ".codex")
		}
	}
	return filepath.Join(dir, cf)
}

// LoginCommand is the operator guidance for provisioning a profile.
func (p Profile) LoginCommand(lane string) string {
	switch lane {
	case "claude":
		return fmt.Sprintf(`set CLAUDE_CONFIG_DIR=%s && claude /login`, p.Home)
	case "codex":
		return fmt.Sprintf(`set CODEX_HOME=%s && codex login`, p.Home)
	}
	return ""
}
