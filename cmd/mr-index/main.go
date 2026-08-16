// Command mr-index builds and refreshes the on-disk skill index that mr-hook
// loads. Build embeds all skills; refresh re-embeds only changed ones.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/embedtpl"
	"github.com/dmmdea/meta-router/internal/index"
	"github.com/dmmdea/meta-router/internal/roots"
)

type config struct {
	cmd        string
	skillRoots string
	endpoint   string
	out        string
	force      bool
	embedModel string
	tpl        string
}

func parseArgs(argv []string) (config, error) {
	if len(argv) == 0 {
		return config{}, fmt.Errorf("usage: mr-index <build|refresh> [flags]")
	}
	cmd := argv[0]
	if cmd != "build" && cmd != "refresh" {
		return config{}, fmt.Errorf("unknown subcommand %q (want build|refresh)", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	skillRoots := fs.String("skill-roots", "", "comma-separated skill root dirs (default: roots.json / auto-discovery of ~/.claude/skills + installed plugin packs)")
	endpoint := fs.String("endpoint", "", "embedder endpoint; empty = resolve for this machine ($MR_EMBED_ENDPOINT, ~/.meta-router/endpoints.json, then the built-in :11436→:18793 failover chain). Setting it pins that endpoint exactly.")
	out := fs.String("out", "", "index path (default ~/.meta-router/index.json)")
	force := fs.Bool("force", false, "refresh: allow removing more than 30% of existing entries")
	embedModel := fs.String("embed-model", "embeddinggemma", "build only: embedding model to request and record in the index identity")
	tpl := fs.String("tpl", "", `build only: embedding template version to apply (e.g. "tpl1"); empty = untemplated raw text (current production behavior). Recorded in the index identity so mr-hook templates queries to match — deploy new binaries fleet-wide BEFORE building a templated index, or older hooks will query it raw.`)
	if err := fs.Parse(argv[1:]); err != nil {
		return config{}, err
	}
	cfg := config{cmd: cmd, skillRoots: *skillRoots, endpoint: *endpoint, out: *out, force: *force,
		embedModel: *embedModel, tpl: *tpl}
	// refresh PRESERVES the index's recorded identity — accepting these flags
	// there would imply a migration that silently never happens (or worse,
	// one that half-happens). Changing model or template is a rebuild. The
	// check is on flag PRESENCE (fs.Visit), not value: `refresh -embed-model
	// embeddinggemma` at the default value is still an operator asking for a
	// migration that won't run, and a value check silently inverts if the
	// default ever changes (review 2026-08-16).
	if cmd == "refresh" {
		var visitErr error
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "tpl" || f.Name == "embed-model" {
				visitErr = fmt.Errorf("-%s applies to build only: refresh preserves the index's recorded model/template; use `mr-index build` to change it", f.Name)
			}
		})
		if visitErr != nil {
			return config{}, visitErr
		}
	}
	return cfg, nil
}

// buildSpec resolves the -embed-model/-tpl flags to the embedding spec a
// build will record. An unknown (model, tpl) pair is a hard error: recording
// an identity nobody can query would brick the hook (it refuses unknown
// templates, fail-open) on every prompt until a rebuild.
func buildSpec(cfg config) (embedtpl.Spec, error) {
	if cfg.tpl == "" {
		return embedtpl.Raw(cfg.embedModel), nil
	}
	if s, ok := embedtpl.Lookup(cfg.embedModel, cfg.tpl); ok {
		return s, nil
	}
	return embedtpl.Spec{}, fmt.Errorf("no template %q for model %q in this binary's registry (see internal/embedtpl)", cfg.tpl, cfg.embedModel)
}

// resolveRoots returns the harvest root set for this run.
//   - -skill-roots given: use exactly those paths (pack = dir basename),
//     never touching roots.json — the explicit-flag escape hatch.
//   - build (no flag): re-discover (user skills + installed plugin packs),
//     carry over operator-owned roots per the ownership rule below, and
//     persist to roots.json next to the index.
//   - refresh (no flag): read roots.json; if ABSENT, discover and create it
//     (this is what lets the SessionStart hook run `mr-index refresh` with no
//     flags and still see the full set). A file that exists but is INVALID is
//     fatal for both commands — see the comment at the Load call.
func resolveRoots(cfg config, outPath string) ([]catalog.Root, []string, error) {
	var staleNotes []string
	var deadPacks []string
	if cfg.skillRoots != "" {
		var rs []catalog.Root
		for _, p := range strings.Split(cfg.skillRoots, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			rs = append(rs, catalog.Root{Path: p, Pack: filepath.Base(filepath.Clean(p))})
		}
		if len(rs) == 0 {
			return nil, nil, fmt.Errorf("-skill-roots given but empty")
		}
		return rs, nil, nil
	}
	rootsPath := roots.ConfigPathFor(outPath)
	// A roots.json that EXISTS but is invalid (parse error, unknown kind) is
	// FATAL for both commands — never "warn and rediscover". Rediscovery ends
	// in roots.Save, which OVERWRITES the operator's file: a typo'd kind would
	// be repaid with the silent destruction of the very edits that carried it
	// (review 2026-07-30 — this exact path also lost hand-added flat roots).
	// Only a genuinely absent file falls through to discovery.
	existing, loadErr := roots.Load(rootsPath)
	if loadErr != nil && !os.IsNotExist(loadErr) {
		return nil, nil, fmt.Errorf("%v — fix or delete %s (refusing to rediscover: that would overwrite your edits)", loadErr, rootsPath)
	}
	if cfg.cmd == "refresh" && loadErr == nil {
		// Ownership classification needs the claudeDir boundary. If the home
		// dir cannot be resolved we cannot classify, so keep the historical
		// behavior (reuse) rather than rediscovering on a guess.
		cd, cderr := roots.DefaultClaudeDir()
		if cderr != nil {
			// Reuse is the safe branch — without the boundary we cannot
			// classify ownership, and rediscovering on a guess ends in Save
			// overwriting the operator's file. But it silently reinstates the
			// pre-fix behavior, so it must not be silent.
			n := fmt.Sprintf("roots.json: cannot resolve home dir (%v) — reusing roots.json UNCHECKED; root decay cannot be detected this run", cderr)
			fmt.Fprintln(os.Stderr, n)
			return existing, []string{n}, nil
		}
		reuse, notes, dead := reuseRoots(existing, cd)
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, n)
		}
		if reuse {
			return existing, notes, nil
		}
		staleNotes = notes
		deadPacks = dead
		// Fall through to rediscovery. The carry-over below preserves every
		// operator-owned root, so this cannot destroy hand edits.
	}
	claudeDir, err := roots.DefaultClaudeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot resolve home dir: %v", err)
	}
	rs := roots.Discover(claudeDir)
	if len(rs) == 0 {
		// Carry the notes out with the error: this exit path skips runRefresh
		// entirely, so main must write the status line or the run leaves NO
		// durable record — the failure mode this change exists to end.
		return nil, staleNotes, fmt.Errorf("no skill roots found under %s", claudeDir)
	}
	// OWNERSHIP RULE (review 2026-07-30 MAJOR + closure-audit MINOR): discovery
	// owns the ~/.claude space — a skills root under claudeDir that discovery
	// no longer finds is an uninstalled pack and SHOULD drop. The operator owns
	// everything else: flat roots (Discover can never emit a kind) and any
	// hand-added root outside claudeDir exist only because a human put them in
	// roots.json, so a build that dropped them would silently destroy edits —
	// the first review round proved that for flat roots (enablement AND all 46
	// index entries gone in one stroke), and the closure audit showed the same
	// shape for outside-tree skills roots. Carried entries are deduped by
	// cleaned path and appended after discovered roots (flat-last also matches
	// HarvestRoots' skills-first dedup ordering).
	if loadErr == nil {
		seen := make(map[string]bool, len(rs))
		for _, r := range rs {
			seen[filepath.Clean(r.Path)] = true
		}
		cleanClaude := filepath.Clean(claudeDir) + string(filepath.Separator)
		for _, r := range existing {
			if seen[filepath.Clean(r.Path)] {
				continue
			}
			flat := r.Kind == catalog.KindCommands || r.Kind == catalog.KindAgents
			outside := !strings.HasPrefix(filepath.Clean(r.Path)+string(filepath.Separator), cleanClaude)
			if flat || outside {
				rs = append(rs, r)
			}
		}
	}
	// Rediscovery is a CLAIM until checked. If a pack whose root vanished did
	// not come back — plugin mid-extraction, a version dir not yet populated,
	// a stale installed_plugins.json — then persisting this set deletes the
	// only record that the pack ever existed, and refresh can never notice it
	// again (no dead root left to trip on). Review reproduced that: three
	// consecutive silent ok:true runs, the last with the plugin fully healthy
	// on disk. So: use the rediscovered set for THIS run, but do not persist
	// it. The next refresh re-detects and retries, which is self-healing.
	if missing := packsMissing(deadPacks, rs); len(missing) > 0 {
		note := fmt.Sprintf("roots.json: pack(s) %s did NOT come back from rediscovery — their skills are dropped this run and roots.json is left UNCHANGED so the next refresh retries; run `mr-index build` once the plugin finishes installing",
			strings.Join(missing, ", "))
		fmt.Fprintln(os.Stderr, note)
		return rs, append(staleNotes, note), nil
	}
	if err := roots.Save(rootsPath, rs); err != nil {
		// Persisting is best-effort: an unwritable roots.json must not block
		// indexing; the next run just rediscovers.
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", rootsPath, err)
	}
	// Rediscovery also DROPS roots.json entries that still exist but are no
	// longer discovered. That is the ownership rule working, but on an
	// unattended refresh it must not be silent.
	for _, note := range droppedEntryNotes(existing, rs) {
		fmt.Fprintln(os.Stderr, note)
		staleNotes = append(staleNotes, note)
	}
	return rs, staleNotes, nil
}

// packsMissing returns the packs from dead that no root in rs provides.
func packsMissing(dead []string, rs []catalog.Root) []string {
	if len(dead) == 0 {
		return nil
	}
	have := make(map[string]bool, len(rs))
	for _, r := range rs {
		have[r.Pack] = true
	}
	var out []string
	for _, p := range dead {
		if !have[p] {
			out = append(out, p)
		}
	}
	return out
}

// droppedEntryNotes reports roots.json entries that rediscovery removed even
// though their directory still exists — a live hand-added skills root inside
// ~/.claude is discovery-owned by the ownership rule, so an unrelated pack
// dying silently takes it with them unless we say so.
func droppedEntryNotes(before, after []catalog.Root) []string {
	kept := make(map[string]bool, len(after))
	for _, r := range after {
		kept[filepath.Clean(r.Path)] = true
	}
	var out []string
	for _, r := range before {
		p := filepath.Clean(r.Path)
		if kept[p] || !isDirPath(p) {
			continue // already reported as stale, or legitimately gone
		}
		out = append(out, fmt.Sprintf("roots.json: entry %q (%s) still exists but is no longer discovered — removed from roots.json and its skills leave the index",
			r.Pack, r.Path))
	}
	return out
}

func isDirPath(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// reuseRoots reports whether `refresh` may reuse roots.json verbatim, plus one
// note per root that no longer exists.
//
// Reusing unconditionally is what silently decayed the production index: plugin
// cache roots carry a VERSION in their path, so an update strands the old path
// and every skill in that pack leaves the index without a word. Nothing else
// catches it — index.Refresh's removal guard only trips past 30% and the real
// decay arrived in smaller steps.
//
// Only a vanished DISCOVERY-owned root forces rediscovery. Operator-owned roots
// (flat roots, anything outside ~/.claude) are reported but never dropped:
// a human put them there, and rediscovery would overwrite that.
//
// The notes are RETURNED rather than printed so the caller can put them in
// refresh.log as well as stderr. A refresh driven by the SessionStart hook has
// nobody watching stderr — that is precisely how this decay ran unnoticed for
// weeks — so the durable log is the channel that actually reaches a human.
//
// deadPacks names the discovery-owned packs that vanished, so the caller can
// check afterwards whether rediscovery actually replaced them.
func reuseRoots(existing []catalog.Root, claudeDir string) (reuse bool, notes []string, deadPacks []string) {
	dead, operatorDead, unknown := roots.Stale(existing, claudeDir)
	for _, u := range unknown {
		// Report the REAL error. "no longer exists" would send a human hunting
		// an uninstall that never happened, when the truth is a deny ACE or an
		// unreachable share.
		notes = append(notes, fmt.Sprintf("roots.json: root %q could not be read (%s): %v — keeping it and NOT rediscovering; liveness is unknown, not absent",
			u.Root.Pack, u.Root.Path, u.Err))
	}
	for _, r := range operatorDead {
		// Name the index loss too. "keeping it" describes the roots.json entry
		// and is the reassuring half: the skills under that root harvest to
		// nothing and leave the index on this very run.
		notes = append(notes, fmt.Sprintf("roots.json: %s root %q no longer exists (%s) — its skills leave the index this run; the entry is kept because it is operator-owned, remove it yourself if the pack is gone",
			ownerLabel(r), r.Pack, r.Path))
	}
	for _, r := range dead {
		notes = append(notes, fmt.Sprintf("roots.json: pack %q root no longer exists (%s) — rediscovering",
			r.Pack, r.Path))
		deadPacks = append(deadPacks, r.Pack)
	}
	return len(dead) == 0, notes, deadPacks
}

func ownerLabel(r catalog.Root) string {
	if r.Kind == catalog.KindCommands || r.Kind == catalog.KindAgents {
		return "flat"
	}
	return "out-of-tree"
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	outPath := cfg.out
	if outPath == "" {
		outPath, err = index.DefaultIndexPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	rs, staleNotes, err := resolveRoots(cfg, outPath)
	if err != nil {
		// This path exits BEFORE runRefresh, where all the logging lives. Left
		// alone it would produce a refresh that decays the index and writes
		// nothing durable — worse than the pre-fix behavior, which at least
		// reached the removal guard and logged a failure.
		if cfg.cmd == "refresh" {
			now := time.Now()
			_ = appendRefreshStatus(filepath.Join(filepath.Dir(outPath), "refresh.log"), refreshStatus{
				Ts: now.Format(time.RFC3339), TsUnix: now.Unix(),
				OK: false, Error: err.Error(), StaleRoots: staleNotes,
			})
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch cfg.cmd {
	case "build":
		spec, err := buildSpec(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		skills, err := index.HarvestSkills(rs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harvest: %v\n", err)
			os.Exit(1)
		}
		idx, err := index.Build(skills, cfg.endpoint, 60*time.Second, spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build: %v\n", err)
			os.Exit(1)
		}
		if _, err := index.RotateBackup(outPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: backup: %v\n", err)
		}
		if err := idx.Save(outPath); err != nil {
			fmt.Fprintf(os.Stderr, "save: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("built %d skills (dim %d, identity %s) from %d roots → %s\n", len(idx.Entries), idx.Dim, idx.Model, len(rs), outPath)
	case "refresh":
		runRefresh(cfg, rs, outPath, staleNotes)
	}
}

// runRefresh is the refresh subcommand body; split out so the status-line and
// guard logic stay testable and every exit path is logged.
func runRefresh(cfg config, rs []catalog.Root, outPath string, staleNotes []string) {
	start := time.Now()
	// StaleRoots rides on EVERY status line this run writes, including the
	// failure paths: a refresh that died after noticing its roots had vanished
	// is exactly the case where the note matters most.
	st := refreshStatus{TsUnix: start.Unix(), Ts: start.Format(time.RFC3339), Forced: cfg.force, StaleRoots: staleNotes}
	logPath := filepath.Join(filepath.Dir(outPath), "refresh.log")
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		st.OK = false
		st.Error = msg
		st.DurationMs = time.Since(start).Milliseconds()
		if err := appendRefreshStatus(logPath, st); err != nil {
			fmt.Fprintf(os.Stderr, "warning: refresh.log: %v\n", err)
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}

	idx, err := index.Load(outPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fail("load: %v", err)
		}
		// no index yet → build fresh. Always untemplated embeddinggemma: the
		// -tpl/-embed-model flags are build-only (parseArgs rejects them on
		// refresh), so a refresh-created index is the legacy identity; a
		// templated index only ever comes from an explicit `build -tpl`.
		skills, herr := index.HarvestSkills(rs)
		if herr != nil {
			fail("harvest: %v", herr)
		}
		idx, err = index.Build(skills, cfg.endpoint, 60*time.Second, embedtpl.Raw(cfg.embedModel))
		if err != nil {
			fail("build: %v", err)
		}
		if err := idx.Save(outPath); err != nil {
			fail("save: %v", err)
		}
		st.OK = true
		st.Identity = idx.Model
		st.EntriesAfter = len(idx.Entries)
		st.Added = len(idx.Entries)
		st.Reembedded = len(idx.Entries)
		st.DurationMs = time.Since(start).Milliseconds()
		if err := appendRefreshStatus(logPath, st); err != nil {
			fmt.Fprintf(os.Stderr, "warning: refresh.log: %v\n", err)
		}
		fmt.Printf("no prior index; built %d skills → %s\n", len(idx.Entries), outPath)
		return
	}
	st.EntriesBefore = len(idx.Entries)
	// Every row from here carries the identity this run operated on (or
	// refused on) — refresh.log must never be identity-blind.
	st.Identity = idx.Model

	// Refresh embeds under the index's OWN recorded identity. An identity
	// this binary cannot resolve (a template version it does not know, or a
	// templated identity whose guard says the vectors were written raw by a
	// pre-template binary) is fatal BEFORE any harvest or embed: proceeding
	// would mix vector spaces inside one index — update the binary or
	// rebuild instead.
	spec, err := idx.SpecForIndex()
	if err != nil {
		fail("refresh: %v", err)
	}

	cur, err := index.HarvestSkills(rs)
	if err != nil {
		fail("harvest: %v", err)
	}
	plan := idx.PlanRefresh(cur, spec)
	st.Added, st.Removed, st.Reembedded = plan.Added, len(plan.RemovedIDs), plan.Reembeds()

	// Removal guard: a wrong roots set / empty harvest must not silently gut
	// the index. Refuse mass removals unless the operator says --force.
	if !cfg.force && index.RemovalExceeds(len(idx.Entries), len(plan.RemovedIDs), 0.30) {
		fmt.Fprintf(os.Stderr, "refusing to remove %d of %d entries (>30%%) without --force. Would remove:\n", len(plan.RemovedIDs), len(idx.Entries))
		for _, id := range plan.RemovedIDs {
			fmt.Fprintf(os.Stderr, "  - %s\n", id)
		}
		fail("refresh aborted: would remove %d/%d entries (>30%%); rerun with --force if intended", len(plan.RemovedIDs), len(idx.Entries))
	}

	if err := idx.ApplyRefresh(plan, cfg.endpoint, 60*time.Second); err != nil {
		fail("refresh: %v", err)
	}
	// MR-16: keep exactly one dated .bak of the index being replaced.
	if _, err := index.RotateBackup(outPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: backup: %v\n", err)
	}
	if err := idx.Save(outPath); err != nil {
		fail("save: %v", err)
	}
	st.OK = true
	st.EntriesAfter = len(idx.Entries)
	st.DurationMs = time.Since(start).Milliseconds()
	if err := appendRefreshStatus(logPath, st); err != nil {
		fmt.Fprintf(os.Stderr, "warning: refresh.log: %v\n", err)
	}
	fmt.Printf("refreshed: +%d ~%d -%d (now %d skills) → %s\n", plan.Added, plan.Updated, len(plan.RemovedIDs), len(idx.Entries), outPath)
}
