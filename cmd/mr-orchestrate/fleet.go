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
	Unstamped bool `json:"unstamped"`
	Err      string `json:"error,omitempty"`
}

// selfBuild reports the running orchestrator's own build identity.
func selfBuild() (version, revision string, dirty bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version, "", false
	}
	version = bi.Main.Version
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return version, revision, dirty
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
	want := shortRev(selfRev)
	if *expect != "" {
		want = shortRev(*expect)
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
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				fb.Revision = shortRev(s.Value)
			case "vcs.modified":
				fb.Dirty = s.Value == "true"
			}
		}
		// Stale iff we know the reference and this binary's revision, and differ.
		fb.Unstamped = fb.Revision == ""
		fb.Stale = want != "" && fb.Revision != "" && fb.Revision != want
		out = append(out, fb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	staleCount, dirtyCount, unstampedCount := 0, 0, 0
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
	}
	undetermined := want == ""
	note := "deployed binaries are compared on BUILD REVISION (versions differ per binary by design). Rebuild the fleet from HEAD when any is stale."
	switch {
	case undetermined:
		note = "STALENESS UNDETERMINED: this binary carries no VCS stamp (Go omits it when building from a git worktree). Pass -expect <revision> to compare."
	case unstampedCount > 0:
		note = "UNVERIFIABLE: " + fmt.Sprint(unstampedCount) + " deployed binary(ies) carry no VCS stamp — provenance unknown, treated as not-current. Rebuild them from a normal checkout (Go omits stamping in a git worktree)."
	case staleCount == 0 && dirtyCount == 0:
		note = "fleet is uniform at revision " + want + "."
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
		Note         string        `json:"note"`
	}{dir, selfVer, shortRev(selfRev), selfDirty, want, undetermined, out, staleCount, dirtyCount, unstampedCount, note}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	if *strict {
		if undetermined {
			return fmt.Errorf("fleet staleness undetermined: no reference revision (pass -expect)")
		}
		if staleCount > 0 || dirtyCount > 0 || unstampedCount > 0 {
			return fmt.Errorf("fleet not current: %d stale, %d dirty, %d unstamped", staleCount, dirtyCount, unstampedCount)
		}
	}
	return nil
}
