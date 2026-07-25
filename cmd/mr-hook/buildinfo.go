package main

import (
	"runtime/debug"
	"strings"
)

// buildRev is injected at deploy time with
// -ldflags "-X main.buildRev=$(git rev-parse HEAD)". Go omits AUTOMATIC vcs
// stamping for worktree builds, which is how this project deploys, so the
// injected value is the one that actually answers "what am I".
var buildRev = ""

// buildRevision reports this binary's embedded VCS revision (and dirty flag),
// the only reliable way to ask a DEPLOYED binary whether it is current.
// Sibling binaries are versioned independently by design, so a version-string
// comparison across the fleet is meaningless — the build revision is not.
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		if buildRev != "" {
			return buildRev
		}
		return "unknown"
	}
	rev, dirty := buildRev, false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if rev == "" {
				rev = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		case "-ldflags":
			if rev == "" {
				if i := strings.Index(s.Value, "main.buildRev="); i >= 0 {
					v := s.Value[i+len("main.buildRev="):]
					if j := strings.IndexAny(v, " \"'"); j >= 0 {
						v = v[:j]
					}
					rev = v
				}
			}
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
