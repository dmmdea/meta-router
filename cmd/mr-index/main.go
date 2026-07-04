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
	"github.com/dmmdea/meta-router/internal/index"
	"github.com/dmmdea/meta-router/internal/roots"
)

type config struct {
	cmd        string
	skillRoots string
	endpoint   string
	out        string
	force      bool
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
	endpoint := fs.String("endpoint", "http://127.0.0.1:11436", "embedder endpoint")
	out := fs.String("out", "", "index path (default ~/.meta-router/index.json)")
	force := fs.Bool("force", false, "refresh: allow removing more than 30% of existing entries")
	if err := fs.Parse(argv[1:]); err != nil {
		return config{}, err
	}
	return config{cmd: cmd, skillRoots: *skillRoots, endpoint: *endpoint, out: *out, force: *force}, nil
}

// resolveRoots returns the harvest root set for this run.
//   - -skill-roots given: use exactly those paths (pack = dir basename),
//     never touching roots.json — the explicit-flag escape hatch.
//   - build (no flag): always re-discover (user skills + installed plugin
//     packs) and persist to roots.json next to the index, so a manual build
//     also refreshes the recorded set.
//   - refresh (no flag): read roots.json; if absent or unreadable, discover
//     and create it. This is what lets the SessionStart hook run
//     `mr-index refresh` with no flags and still see the full set.
func resolveRoots(cfg config, outPath string) ([]catalog.Root, error) {
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
			return nil, fmt.Errorf("-skill-roots given but empty")
		}
		return rs, nil
	}
	rootsPath := roots.ConfigPathFor(outPath)
	if cfg.cmd == "refresh" {
		rs, err := roots.Load(rootsPath)
		if err == nil {
			return rs, nil
		}
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: %v — rediscovering roots\n", err)
		}
	}
	claudeDir, err := roots.DefaultClaudeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve home dir: %v", err)
	}
	rs := roots.Discover(claudeDir)
	if len(rs) == 0 {
		return nil, fmt.Errorf("no skill roots found under %s", claudeDir)
	}
	if err := roots.Save(rootsPath, rs); err != nil {
		// Persisting is best-effort: an unwritable roots.json must not block
		// indexing; the next run just rediscovers.
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", rootsPath, err)
	}
	return rs, nil
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
	rs, err := resolveRoots(cfg, outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch cfg.cmd {
	case "build":
		skills, err := index.HarvestSkills(rs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harvest: %v\n", err)
			os.Exit(1)
		}
		idx, err := index.Build(skills, cfg.endpoint, 60*time.Second)
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
		fmt.Printf("built %d skills (dim %d) from %d roots → %s\n", len(idx.Entries), idx.Dim, len(rs), outPath)
	case "refresh":
		runRefresh(cfg, rs, outPath)
	}
}

// runRefresh is the refresh subcommand body; split out so the status-line and
// guard logic stay testable and every exit path is logged.
func runRefresh(cfg config, rs []catalog.Root, outPath string) {
	start := time.Now()
	st := refreshStatus{TsUnix: start.Unix(), Ts: start.Format(time.RFC3339), Forced: cfg.force}
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
		// no index yet → build fresh
		skills, herr := index.HarvestSkills(rs)
		if herr != nil {
			fail("harvest: %v", herr)
		}
		idx, err = index.Build(skills, cfg.endpoint, 60*time.Second)
		if err != nil {
			fail("build: %v", err)
		}
		if err := idx.Save(outPath); err != nil {
			fail("save: %v", err)
		}
		st.OK = true
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

	cur, err := index.HarvestSkills(rs)
	if err != nil {
		fail("harvest: %v", err)
	}
	plan := idx.PlanRefresh(cur)
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
