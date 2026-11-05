// Command mr-outcomes joins mr-hook usage rows (usage.jsonl) with Skill-tool
// invocations (outcomes.jsonl, written by a PostToolUse hook) and reports whether
// the skill hint is followed, per holdout arm.
//
// outcomes.jsonl format (one JSON object per line):
//
//	{"ts_unix":1751600000,"skill":"superpowers:brainstorming","session_id":"...","prompt_id":"...","tool_use_id":"..."}
//
// where skill is the invocable skill name exactly as the Skill tool receives it (and
// exactly as mr-hook surfaces it). session_id and prompt_id make the join exact; a row
// without a session id cannot be attributed and is counted and skipped. This tool only
// READS the files.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dmmdea/meta-router/internal/usagelog"
)

func main() {
	usagePath := flag.String("usage", "", "usage log path (default ~/.meta-router/usage.jsonl)")
	outcomesPath := flag.String("outcomes", "", "outcomes log path (default ~/.meta-router/outcomes.jsonl)")
	days := flag.Int("days", 30, "only rows from the last N days; 0 = all")
	windowMin := flag.Int("window-min", 30, "when set explicitly, also print the session-blind time-window report: an invocation within N minutes after a surfacing counts as a hit")
	flag.Parse()
	windowGiven := false
	flag.Visit(func(f *flag.Flag) { windowGiven = windowGiven || f.Name == "window-min" })

	up := *usagePath
	if up == "" {
		p, err := usagelog.DefaultLogPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		up = p
	}
	op := *outcomesPath
	if op == "" {
		op = filepath.Join(filepath.Dir(up), "outcomes.jsonl")
	}

	recs, err := usagelog.ReadRecords(up)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", up, err)
		os.Exit(1)
	}
	outs, err := usagelog.ReadOutcomes(op)
	if err != nil {
		if os.IsNotExist(err) {
			// Expected until the PostToolUse hook is wired: report the
			// surfacing side with zero outcomes rather than failing.
			fmt.Printf("note: %s not found (PostToolUse outcome hook not wired yet); joining against 0 invocations\n\n", op)
			outs = nil
		} else {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", op, err)
			os.Exit(1)
		}
	}
	if *days > 0 {
		cut := time.Now().AddDate(0, 0, -*days).Unix()
		recs = keep(recs, func(r usagelog.Record) bool { return r.TsUnix >= cut })
		outs = keep(outs, func(o usagelog.Outcome) bool { return o.TsUnix >= cut })
		fmt.Printf("window: last %d days\n", *days)
	} else {
		fmt.Printf("window: all rows\n")
	}

	ex := usagelog.JoinExact(recs, outs)
	fmt.Printf("\nfollow-through (a hinted prompt is followed when its session then calls one of its skills; exact session join)\n")
	fmt.Printf("| arm     | prompts | followed | rate   |\n|---------|---------|----------|--------|\n")
	fmt.Printf("| treat   | %7d | %8d | %5.1f%% |\n", ex.Treat.Prompts, ex.Treat.Followed, 100*ex.Treat.Rate())
	fmt.Printf("| holdout | %7d | %8d | %5.1f%% |\n", ex.Holdout.Prompts, ex.Holdout.Followed, 100*ex.Holdout.Rate())
	if ex.NoArm.Prompts > 0 {
		// Rows from before the holdout, or while it was off: shown, never part of the comparison.
		fmt.Printf("| no arm  | %7d | %8d | %5.1f%% |\n", ex.NoArm.Prompts, ex.NoArm.Followed, 100*ex.NoArm.Rate())
	}
	if ex.Treat.Prompts > 0 && ex.Holdout.Prompts > 0 {
		fmt.Printf("treat minus holdout: %+.1f points\n", 100*(ex.Treat.Rate()-ex.Holdout.Rate()))
	} else {
		fmt.Printf("treat minus holdout: n/a (needs prompts in both arms)\n")
	}
	never := 0.0
	if ex.Calls > 0 {
		never = 100 * float64(ex.Calls-ex.CallsHinted) / float64(ex.Calls)
	}
	fmt.Printf("skill calls: %d, hinted: %d, never hinted: %.1f%%\n", ex.Calls, ex.CallsHinted, never)
	fmt.Printf("outcome rows skipped (no session id): %d\n", ex.OutcomesNoSession)
	printSkills(ex.PerSkill)

	if !windowGiven {
		return
	}
	rep := usagelog.JoinOutcomes(recs, outs, time.Duration(*windowMin)*time.Minute)
	// This hit-rate is a TIME-WINDOW join and is session-BLIND: an invocation in one
	// session can be credited to a surfacing in another. Measured on a real fleet, 72.4% of
	// active 10-minute buckets had more than one session running, which inflated a recorded
	// figure from ~23% to a claimed 39.7%. The exact report above is the one to read.
	fmt.Printf("\nNOTE: the window report below is a session-BLIND join and reads HIGH when sessions\n" +
		"      run concurrently; the exact report above is the one to trust.\n")
	fmt.Printf("usage records: %d\nsurfacings (>=1 skill): %d\ninvocations logged: %d\nhits (surfaced skill invoked within %dm): %d\nhit-rate: %.3f\n",
		rep.Records, rep.Surfacings, len(outs), *windowMin, rep.Hits, rep.HitRate())
}

// keep returns the items for which f is true.
func keep[T any](xs []T, f func(T) bool) []T {
	var out []T
	for _, x := range xs {
		if f(x) {
			out = append(out, x)
		}
	}
	return out
}

// printSkills prints the per-skill table of the rows that showed a list, most surfaced first.
func printSkills(per map[string]*usagelog.SkillStat) {
	if len(per) == 0 {
		return
	}
	type row struct {
		id string
		st *usagelog.SkillStat
	}
	rows := make([]row, 0, len(per))
	for id, st := range per {
		rows = append(rows, row{id, st})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].st.Surfaced != rows[j].st.Surfaced {
			return rows[i].st.Surfaced > rows[j].st.Surfaced
		}
		return rows[i].id < rows[j].id
	})
	fmt.Printf("\n| skill                                    | surfaced | invoked |\n")
	fmt.Printf("|------------------------------------------|----------|---------|\n")
	for _, r := range rows {
		fmt.Printf("| %-40s | %8d | %7d |\n", r.id, r.st.Surfaced, r.st.Invoked)
	}
}
