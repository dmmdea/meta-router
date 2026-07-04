package usagelog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJoinOutcomes_HitWithinWindow(t *testing.T) {
	recs := []Record{
		{TsUnix: 1000, Surfaced: []string{"gstack-qa", "superpowers:brainstorming"}, Mode: "embed"},
		{TsUnix: 2000, Surfaced: []string{"lavish"}, Mode: "embed"},
		{TsUnix: 3000, Mode: "gated-empty"}, // no surfacing → not counted
	}
	outs := []Outcome{
		{TsUnix: 1100, Skill: "superpowers:brainstorming"}, // within 30min of rec 1
		{TsUnix: 2000 + 1801, Skill: "lavish"},             // 1 s past the 30min window
		{TsUnix: 900, Skill: "gstack-qa"},                  // BEFORE surfacing → ignored
	}
	rep := JoinOutcomes(recs, outs, 30*time.Minute)
	if rep.Records != 3 || rep.Surfacings != 2 {
		t.Fatalf("records/surfacings wrong: %+v", rep)
	}
	if rep.Hits != 1 {
		t.Fatalf("hits=%d, want 1 (brainstorming within window; lavish outside; qa was before)", rep.Hits)
	}
	if got := rep.HitRate(); got != 0.5 {
		t.Fatalf("hit-rate=%v, want 0.5", got)
	}
	if st := rep.PerSkill["superpowers:brainstorming"]; st == nil || st.Surfaced != 1 || st.Invoked != 1 {
		t.Fatalf("per-skill brainstorming wrong: %+v", st)
	}
	if st := rep.PerSkill["gstack-qa"]; st == nil || st.Surfaced != 1 || st.Invoked != 0 {
		t.Fatalf("per-skill gstack-qa wrong: %+v", st)
	}
	if st := rep.PerSkill["lavish"]; st == nil || st.Surfaced != 1 || st.Invoked != 0 {
		t.Fatalf("per-skill lavish wrong: %+v", st)
	}
}

func TestJoinOutcomes_WindowBoundaryInclusive(t *testing.T) {
	recs := []Record{{TsUnix: 1000, Surfaced: []string{"x"}}}
	outs := []Outcome{{TsUnix: 1000 + 1800, Skill: "x"}} // exactly at the boundary
	rep := JoinOutcomes(recs, outs, 30*time.Minute)
	if rep.Hits != 1 {
		t.Fatal("boundary invocation (ts+window) must count")
	}
}

func TestReadRecordsAndOutcomes_TolerateJunk(t *testing.T) {
	dir := t.TempDir()
	up := filepath.Join(dir, "usage.jsonl")
	content := `{"ts_unix":1,"surfaced":["a"],"mode":"embed"}
not json at all
{"ts_unix":2,"mode":"gated-empty"}
`
	if err := os.WriteFile(up, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadRecords(up)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].TsUnix != 1 || len(recs[0].Surfaced) != 1 {
		t.Fatalf("tolerant read failed: %+v", recs)
	}

	op := filepath.Join(dir, "outcomes.jsonl")
	if err := os.WriteFile(op, []byte(`{"ts_unix":5,"skill":"a"}`+"\n{bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outs, err := ReadOutcomes(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Skill != "a" {
		t.Fatalf("outcomes read failed: %+v", outs)
	}
}
