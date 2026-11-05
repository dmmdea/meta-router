package usagelog

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
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
	// SessionID, PromptID and ToolUseID are the opaque ids Claude Code puts on the
	// PostToolUse payload. With them an outcome is attributed to the prompt that
	// preceded it exactly (JoinExact) instead of by a session-blind time window.
	// All three are omitempty: rows written before they existed leave them absent.
	SessionID string `json:"session_id,omitempty"`
	PromptID  string `json:"prompt_id,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
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

// ArmStat is one holdout arm's follow-through: Prompts that had a skill list (shown, or for the
// holdout arm computed and withheld) and how many of those were Followed by a call of one of
// those skills.
type ArmStat struct{ Prompts, Followed int }

// Rate is Followed / Prompts (0 when there were no prompts).
func (a ArmStat) Rate() float64 {
	if a.Prompts == 0 {
		return 0
	}
	return float64(a.Followed) / float64(a.Prompts)
}

// ExactReport is the session-exact join. Treat and Holdout are the two arms of the holdout;
// NoArm holds the rows written while the holdout was off or before it existed. Those are
// reported on their own and never folded into Treat: they were produced under other settings,
// and most predate the ids an outcome needs to be joined, so mixing them in would lower the
// treat rate for a reason that has nothing to do with the hint. Calls counts outcomes that
// carry a session id and CallsHinted those attributed to a prompt whose shown list held the
// called skill, so Calls-CallsHinted is the share of skill calls nobody hinted.
// OutcomesNoSession counts outcome rows skipped for lacking a session id.
type ExactReport struct {
	Treat, Holdout, NoArm                 ArmStat
	Calls, CallsHinted, OutcomesNoSession int
	PerSkill                              map[string]*SkillStat // rows that showed a list (treat and no-arm)
}

// JoinExact attributes each outcome that has a session id to one usage row of the same session:
// the row with the same prompt_id when the outcome has one that matches, else the latest row of
// that session at or before the outcome's time whose mode is not machine-turn (a notification is
// not a request and never produced a hint). A prompt counts when it had a skill list: Surfaced
// (treat rows and rows without an arm) or WouldSurface (holdout). It is followed when an
// attributed outcome names one of those skills (case-insensitive).
func JoinExact(recs []Record, outs []Outcome) ExactReport {
	rep := ExactReport{PerSkill: map[string]*SkillStat{}}
	bySession := map[string][]int{}
	for i, r := range recs {
		if r.SessionID != "" {
			bySession[r.SessionID] = append(bySession[r.SessionID], i)
		}
	}
	for _, idxs := range bySession {
		sort.SliceStable(idxs, func(a, b int) bool { return recs[idxs[a]].TsUnix < recs[idxs[b]].TsUnix })
	}
	invoked := make([]map[string]bool, len(recs)) // row -> lower-cased skills called after it
	for _, o := range outs {
		if o.SessionID == "" {
			rep.OutcomesNoSession++
			continue
		}
		rep.Calls++
		idxs, row := bySession[o.SessionID], -1
		if o.PromptID != "" {
			for j := len(idxs) - 1; j >= 0 && row < 0; j-- {
				if recs[idxs[j]].PromptID == o.PromptID {
					row = idxs[j]
				}
			}
		}
		for j := len(idxs) - 1; j >= 0 && row < 0; j-- {
			if r := recs[idxs[j]]; r.TsUnix <= o.TsUnix && r.Mode != "machine-turn" {
				row = idxs[j]
			}
		}
		if row < 0 {
			continue
		}
		if containsFold(recs[row].Surfaced, o.Skill) {
			rep.CallsHinted++
		}
		if invoked[row] == nil {
			invoked[row] = map[string]bool{}
		}
		invoked[row][strings.ToLower(o.Skill)] = true
	}
	for i, r := range recs {
		ids, arm, shown := r.Surfaced, &rep.NoArm, true
		switch r.Arm {
		case "treat":
			arm = &rep.Treat
		case "holdout":
			ids, arm, shown = r.WouldSurface, &rep.Holdout, false
		}
		if len(ids) == 0 {
			continue
		}
		arm.Prompts++
		followed := false
		for _, id := range ids {
			hit := invoked[i][strings.ToLower(id)]
			followed = followed || hit
			if shown {
				st := rep.PerSkill[id]
				if st == nil {
					st = &SkillStat{}
					rep.PerSkill[id] = st
				}
				st.Surfaced++
				if hit {
					st.Invoked++
				}
			}
		}
		if followed {
			arm.Followed++
		}
	}
	return rep
}

func containsFold(ids []string, s string) bool {
	for _, id := range ids {
		if strings.EqualFold(id, s) {
			return true
		}
	}
	return false
}
