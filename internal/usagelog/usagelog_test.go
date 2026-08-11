package usagelog

// Session/prompt identity exists because a TIMESTAMP join across concurrent
// sessions is wrong: measured on this fleet, 72.4% of active 10-minute buckets
// have more than one session running (median 2, max 110). A window join over
// session-blind logs silently attributes one session's invocation to another
// session's surfacing — which inflated a recorded live metric from ~23% to a
// claimed 39.7%.
//
// Claude Code passes session_id and prompt_id on the UserPromptSubmit payload
// (prompt_id since v2.1.196; this fleet runs 2.1.219+), so the join can be an
// EXACT composite key instead of a content hash. Both are opaque identifiers,
// not prompt content: the no-prompt-text privacy posture is unchanged.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppend_TwoLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := Append(p, Record{TsUnix: 1, PromptHash: "h", Surfaced: []string{"skills:a"}, Mode: "hybrid"}); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, Record{TsUnix: 2, Mode: "gated-empty"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var n int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d not valid JSON: %v", n, err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("got %d lines want 2", n)
	}
}

// The identifiers must round-trip, and must be OMITTED when absent so older
// Claude Code builds (or any payload without them) do not write empty keys that
// a downstream join would treat as a real, shared session.
func TestRecordCarriesSessionAndPromptID(t *testing.T) {
	b, err := json.Marshal(Record{SessionID: "sess-1", PromptID: "prompt-1"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"session_id":"sess-1"`) || !strings.Contains(s, `"prompt_id":"prompt-1"`) {
		t.Fatalf("identifiers must be logged: %s", s)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SessionID != "sess-1" || back.PromptID != "prompt-1" {
		t.Fatalf("round trip lost the identifiers: %+v", back)
	}

	empty, err := json.Marshal(Record{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "session_id") || strings.Contains(string(empty), "prompt_id") {
		t.Fatalf("absent identifiers must be omitted, not written empty: %s", empty)
	}
}

// The privacy posture is the reason this log is trustworthy: names, numbers and
// opaque ids only. A Record must never be able to carry prompt text.
func TestRecordStillCarriesNoPromptText(t *testing.T) {
	b, err := json.Marshal(Record{
		PromptHash: HashPrompt("a very secret prompt"),
		PromptLen:  20,
		SessionID:  "sess-1",
		PromptID:   "prompt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "a very secret prompt") {
		t.Fatalf("prompt text must never reach the log: %s", b)
	}
}

func TestHashPrompt_Stable(t *testing.T) {
	if HashPrompt("x") != HashPrompt("x") || HashPrompt("x") == HashPrompt("y") {
		t.Fatal("hash not stable/distinct")
	}
	const wantX = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" // sha256("x")
	if got := HashPrompt("x"); got != wantX {
		t.Fatalf("HashPrompt(\"x\") = %s, want %s", got, wantX)
	}
}
