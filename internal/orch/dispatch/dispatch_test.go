package dispatch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d", "dispatch.jsonl")
	r1 := Record{TS: time.Now().UTC(), Lane: "claude", Model: "claude-opus-4-8", OutcomeClass: "ok", Admit: true, AdmitState: "open"}
	r2 := Record{TS: time.Now().UTC(), Lane: "claude", Model: "claude-sonnet-5", OutcomeClass: "rate_limit", Admit: true, AdmitState: "throttled"}
	for _, r := range []Record{r1, r2} {
		if err := Append(p, r); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 || got[0].Model != "claude-opus-4-8" || got[1].OutcomeClass != "rate_limit" {
		t.Fatalf("round trip: %+v", got)
	}
}

// RS9/§6c adherence fields + the S2R-9 replay-substrate fields (desc/quality)
// must survive the receipt round-trip.
func TestAdherenceFieldsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dispatch.jsonl")
	r := Record{TS: time.Now().UTC(), Lane: "glm", Model: "glm-5.2", OutcomeClass: "ok",
		Origin: "mcp", TaskClass: "workhorse-coding",
		RecLane: "glm", RecModel: "glm-5.2", RecRule: "workhorse-coding#1:glm",
		Deviated: false,
		Desc:     "refactor the parser tests", Quality: "good"}
	if err := Append(p, r); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var got Record
	if err := json.Unmarshal(b, &got); err != nil || got.RecRule != "workhorse-coding#1:glm" || got.Origin != "mcp" {
		t.Fatalf("adherence fields must survive the receipt: %+v err=%v", got, err)
	}
	if got.Desc != "refactor the parser tests" || got.Quality != "good" {
		t.Fatalf("S2R-9 desc/quality must survive the receipt: %+v", got)
	}
}

// Slice-3 seam (Task 1): the four strategy fields are additive omitempty — a
// slice-2 line with NONE of them still unmarshals clean, and a strategy line
// round-trips while omitting empties.
func TestRecordStrategyFieldsOmitemptyAndBackCompat(t *testing.T) {
	// A slice-2 line with NONE of the strategy fields still unmarshals.
	old := []byte(`{"ts":"2026-07-01T00:00:00Z","lane":"claude","outcome_class":"ok","admit":true,"admit_state":"open","tokens_in":1,"tokens_out":2,"num_turns":1,"notional_usd":0}`)
	var r Record
	if err := json.Unmarshal(old, &r); err != nil {
		t.Fatalf("old JSONL must still unmarshal: %v", err)
	}
	if r.DispatchID != "" || r.StepID != 0 || r.Deps != nil || r.Attempt != 0 {
		t.Fatal("absent strategy fields must default clean")
	}
	// A strategy line round-trips and OMITS the empties.
	b, _ := json.Marshal(Record{DispatchID: "d1", StepID: 2, Deps: []int{0, 1}, Attempt: 1, OutcomeClass: "ok"})
	if bytes.Contains(b, []byte(`"rec_lane"`)) {
		t.Fatal("empty adherence fields must stay omitted")
	}
	var back Record
	if json.Unmarshal(b, &back); back.DispatchID != "d1" || back.StepID != 2 || back.Attempt != 1 || len(back.Deps) != 2 {
		t.Fatalf("strategy fields must round-trip: %+v", back)
	}
}
