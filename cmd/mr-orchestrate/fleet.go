package main

import (
	"debug/buildinfo"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

// buildRev is injected at build time with
//
//	-ldflags "-X main.buildRev=$(git rev-parse HEAD)"
//
// Go's AUTOMATIC vcs stamping is absent whenever the build happens in a git
// worktree — which is exactly how this project's deploys are built, so relying
// on it alone left the check inert on the only path that matters. The -ldflags
// setting IS recorded in build info, so an injected revision is readable from a
// deployed file with no subprocess.
var buildRev = ""

// fleetBinary is one deployed binary's build identity.
type fleetBinary struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
	Stale    bool   `json:"stale"` // revision differs from the reference
	// Unstamped: the binary carries NO vcs.revision, so its provenance cannot
	// be verified at all. Reporting it as "not stale" would be a false
	// all-clear — it counts against the fleet exactly like a stale one.
	Unstamped bool   `json:"unstamped"`
	Err       string `json:"error,omitempty"`
}

// selfBuild reports the running orchestrator's own build identity.
func selfBuild() (version, revision string, dirty bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version, buildRev, false
	}
	version = bi.Main.Version
	revision = buildRev // injected wins: it survives worktree builds
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if revision == "" {
				revision = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return version, revision, dirty
}

// revFromBuildInfo extracts a deployed binary's revision: the automatic vcs
// stamp when present, else the -ldflags-injected main.buildRev.
func revFromBuildInfo(bi *debug.BuildInfo) (rev string, dirty bool) {
	for _, st := range bi.Settings {
		switch st.Key {
		case "vcs.revision":
			rev = st.Value
		case "vcs.modified":
			dirty = st.Value == "true"
		case "-ldflags":
			if rev == "" {
				if i := strings.Index(st.Value, "main.buildRev="); i >= 0 {
					v := st.Value[i+len("main.buildRev="):]
					if j := strings.IndexAny(v, " \"'"); j >= 0 {
						v = v[:j]
					}
					rev = v
				}
			}
		}
	}
	return rev, dirty
}

// revMatch compares revisions by PREFIX so an operator's 7-char
// `git rev-parse --short` matches a full 40-char stamp. Comparing fixed
// 12-char truncations reported every binary stale for a short sha.
func revMatch(have, want string) bool {
	if have == "" || want == "" {
		return false
	}
	if len(want) <= len(have) {
		return strings.HasPrefix(have, want)
	}
	return strings.HasPrefix(want, have)
}

func shortRev(r string) string {
	if len(r) > 12 {
		return r[:12]
	}
	return r
}

// runFleet reports the build identity of every mr-* binary in the deployed bin
// directory and flags any whose VCS revision differs from this orchestrator's.
//
// This exists because B11 (version parity) is SOURCE-only: it compares VERSION,
// the version literal and the CHANGELOG inside the repo, and never looks at
// what is actually installed. That gap let an admission fix from 2026-07-19
// survive three releases undeployed — mr-hook stayed on a build that rendered
// expired windows as live throttle pressure in every prompt (audit 2026-07-25).
// Sibling binaries are versioned independently by design (an adjudicated
// decision), so the comparison is on build REVISION, never version strings.
//
// Exit 0 always: this is a report, not a gate. `-strict` makes a stale or dirty
// fleet exit 1 so a wrapper can gate on it.
func runFleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ExitOnError)
	binDir := fs.String("bin", "", "deployed bin directory (default ~/.meta-router/bin)")
	strict := fs.Bool("strict", false, "exit 1 when any deployed binary is stale or dirty, or when staleness cannot be determined")
	expect := fs.String("expect", "", "revision every deployed binary must match (default: this orchestrator's own). Required when this binary carries no VCS stamp — e.g. built from a git worktree, where Go omits it")
	_ = fs.Parse(args)

	dir := *binDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".meta-router", "bin")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	selfVer, selfRev, selfDirty := selfBuild()
	// The reference revision: -expect wins, else our own stamp. If NEITHER is
	// known, staleness is UNDETERMINED and must be said so — silently reporting
	// stale=false for everything would be a false all-clear, which is the exact
	// failure class this subcommand exists to end.
	want := selfRev
	if *expect != "" {
		want = *expect // compared by prefix, so a short sha is fine
	}

	var out []fleetBinary
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "mr-") || filepath.Ext(n) != ".exe" {
			continue
		}
		if strings.Contains(n, ".bak") {
			continue
		}
		fb := fleetBinary{Name: n}
		bi, berr := buildinfo.ReadFile(filepath.Join(dir, n))
		if berr != nil {
			fb.Err = berr.Error()
			out = append(out, fb)
			continue
		}
		fb.Version = bi.Main.Version
		full, dirty := revFromBuildInfo(bi)
		fb.Revision = shortRev(full)
		fb.Dirty = dirty
		// Stale iff we know the reference and this binary's revision, and they
		// disagree by prefix.
		fb.Unstamped = full == ""
		fb.Stale = want != "" && full != "" && !revMatch(full, want)
		out = append(out, fb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	staleCount, dirtyCount, unstampedCount, errCount := 0, 0, 0, 0
	for _, b := range out {
		if b.Stale {
			staleCount++
		}
		if b.Dirty {
			dirtyCount++
		}
		if b.Unstamped {
			unstampedCount++
		}
		if b.Err != "" {
			errCount++
		}
	}
	undetermined := want == ""
	note := "deployed binaries are compared on BUILD REVISION by prefix (versions differ per binary by design). Rebuild the fleet when any is stale."
	switch {
	case len(out) == 0:
		note = "NO mr-* BINARIES FOUND in " + dir + " — nothing was verified (wrong -bin, or a non-Windows host where the .exe filter matches nothing)."
	case undetermined:
		note = "STALENESS UNDETERMINED: no reference revision. Pass -expect <revision>, or build with -ldflags \"-X main.buildRev=$(git rev-parse HEAD)\" (Go omits automatic VCS stamping for worktree builds)."
	case errCount > 0:
		note = "UNVERIFIABLE: " + fmt.Sprint(errCount) + " deployed binary(ies) could not be read (deploy in progress, truncated copy) — treated as not-current."
	case unstampedCount > 0:
		note = "UNVERIFIABLE: " + fmt.Sprint(unstampedCount) + " deployed binary(ies) carry no revision — build them with -ldflags \"-X main.buildRev=...\"; treated as not-current."
	case staleCount == 0 && dirtyCount == 0:
		note = "fleet is uniform at revision " + shortRev(want) + "."
	}
	rep := struct {
		BinDir       string        `json:"bin_dir"`
		SelfVer      string        `json:"orchestrator_version"`
		SelfRev      string        `json:"orchestrator_revision"`
		SelfDirty    bool          `json:"orchestrator_dirty"`
		WantRev      string        `json:"expected_revision"`
		Undetermined bool          `json:"staleness_undetermined"`
		Binaries     []fleetBinary `json:"binaries"`
		StaleCount   int           `json:"stale_count"`
		DirtyCount   int           `json:"dirty_count"`
		Unstamped    int           `json:"unstamped_count"`
		Unreadable   int           `json:"unreadable_count"`
		Note         string        `json:"note"`
	}{dir, selfVer, shortRev(selfRev), selfDirty, shortRev(want), undetermined, out, staleCount, dirtyCount, unstampedCount, errCount, note}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	if *strict {
		if len(out) == 0 {
			return fmt.Errorf("no mr-* binaries found in %s: nothing verified", dir)
		}
		if undetermined {
			return fmt.Errorf("fleet staleness undetermined: no reference revision (pass -expect)")
		}
		if staleCount > 0 || dirtyCount > 0 || unstampedCount > 0 || errCount > 0 {
			return fmt.Errorf("fleet not current: %d stale, %d dirty, %d unstamped, %d unreadable",
				staleCount, dirtyCount, unstampedCount, errCount)
		}
	}
	return nil
}
