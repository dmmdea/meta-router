package usagelog

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"time"
)

// Outcome is one Skill-tool invocation, as written (one JSON per line) to
// ~/.meta-router/outcomes.jsonl by a PostToolUse hook:
//
//	{"ts_unix":1751600000,"skill":"superpowers:brainstorming"}
//
// Skill must be the invocable skill name — the same identity mr-hook
// surfaces and logs in Record.Surfaced — so the join is a string equality.
type Outcome struct {
	TsUnix int64  `json:"ts_unix"`
	Skill  string `json:"skill"`
}

// ReadRecords loads a usage.jsonl. Malformed lines are skipped, not fatal:
// the log is append-only across binary versions and a single bad line must
// not blind reporting.
func ReadRecords(path string) ([]Record, error) {
	return readLines[Record](path)
}

// ReadOutcomes loads an outcomes.jsonl (same tolerance as ReadRecords).
func ReadOutcomes(path string) ([]Outcome, error) {
	return readLines[Outcome](path)
}

func readLines[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			continue // tolerate junk lines
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// SkillStat aggregates per-skill surfacing effectiveness.
type SkillStat struct {
	Surfaced int // times mr-hook surfaced this skill
	Invoked  int // times an invocation followed within the window
}

// JoinReport is the surfaced→invoked join result.
type JoinReport struct {
	Records    int // usage records seen
	Surfacings int // records that surfaced at least one skill
	Hits       int // surfacings followed by an invocation of a surfaced skill
	PerSkill   map[string]*SkillStat
}

// HitRate is Hits / Surfacings (0 when nothing was surfaced).
func (r JoinReport) HitRate() float64 {
	if r.Surfacings == 0 {
		return 0
	}
	return float64(r.Hits) / float64(r.Surfacings)
}

// JoinOutcomes matches each surfacing against Skill invocations within
// [ts, ts+window]. Only forward matches count: an invocation BEFORE the
// surfacing cannot have been caused by it.
func JoinOutcomes(recs []Record, outs []Outcome, window time.Duration) JoinReport {
	rep := JoinReport{Records: len(recs), PerSkill: map[string]*SkillStat{}}
	sorted := append([]Outcome(nil), outs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TsUnix < sorted[j].TsUnix })
	winSec := int64(window / time.Second)

	for _, r := range recs {
		if len(r.Surfaced) == 0 {
			continue
		}
		rep.Surfacings++
		// Outcomes inside [r.TsUnix, r.TsUnix+winSec].
		lo := sort.Search(len(sorted), func(i int) bool { return sorted[i].TsUnix >= r.TsUnix })
		invoked := map[string]bool{}
		for i := lo; i < len(sorted) && sorted[i].TsUnix-r.TsUnix <= winSec; i++ {
			invoked[sorted[i].Skill] = true
		}
		hit := false
		for _, id := range r.Surfaced {
			st := rep.PerSkill[id]
			if st == nil {
				st = &SkillStat{}
				rep.PerSkill[id] = st
			}
			st.Surfaced++
			if invoked[id] {
				st.Invoked++
				hit = true
			}
		}
		if hit {
			rep.Hits++
		}
	}
	return rep
}
