// Command mr-outcomes joins mr-hook surfacings (usage.jsonl) with Skill-tool
// invocations (outcomes.jsonl, written by a PostToolUse hook) and reports the
// surfaced→invoked hit-rate — the outcome side of the MR-12 telemetry loop.
//
// outcomes.jsonl format (one JSON object per line):
//
//	{"ts_unix":1751600000,"skill":"superpowers:brainstorming"}
//
// where skill is the invocable skill name exactly as the Skill tool receives
// it (and exactly as mr-hook surfaces it). This tool only READS the file;
// wiring the PostToolUse hook that writes it is a separate deployment step.
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
	windowMin := flag.Int("window-min", 30, "attribution window: an invocation within N minutes after a surfacing counts as a hit")
	flag.Parse()

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

	rep := usagelog.JoinOutcomes(recs, outs, time.Duration(*windowMin)*time.Minute)
	fmt.Printf("usage records: %d\nsurfacings (>=1 skill): %d\ninvocations logged: %d\nhits (surfaced skill invoked within %dm): %d\nhit-rate: %.3f\n",
		rep.Records, rep.Surfacings, len(outs), *windowMin, rep.Hits, rep.HitRate())

	if len(rep.PerSkill) == 0 {
		return
	}
	type row struct {
		id string
		st *usagelog.SkillStat
	}
	rows := make([]row, 0, len(rep.PerSkill))
	for id, st := range rep.PerSkill {
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
