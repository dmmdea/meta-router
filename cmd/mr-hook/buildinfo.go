package main

import "runtime/debug"

// buildRevision reports this binary's embedded VCS revision (and dirty flag),
// the only reliable way to ask a DEPLOYED binary whether it is current.
// Sibling binaries are versioned independently by design, so a version-string
// comparison across the fleet is meaningless — the build revision is not.
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return bi.Main.Version
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return rev + "+dirty"
	}
	return rev
}
