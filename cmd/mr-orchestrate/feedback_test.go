package main

import (
	"strings"
	"testing"
)

// S2R-9: `mr-orchestrate feedback <ts> good|bad` tags a receipt with an
// operator quality verdict — the gold-set/replay label channel. The pure core
// must preserve unmatched lines BYTE-identical (unknown future fields on other
// receipts must never be re-marshalled away).
func TestApplyFeedbackTagsExactlyOneReceipt(t *testing.T) {
	log := strings.Join([]string{
		`{"ts":"2026-07-06T12:00:00Z","lane":"codex","model":"gpt-5.5","outcome_class":"ok","admit":true,"admit_state":"open","tokens_in":1,"tokens_out":1,"num_turns":1,"notional_usd":0,"future_field":"keep-me"}`,
		`{"ts":"2026-07-06T13:00:00Z","lane":"glm","model":"glm-5.2","outcome_class":"ok","admit":true,"admit_state":"open","tokens_in":1,"tokens_out":1,"num_turns":1,"notional_usd":0}`,
	}, "\n") + "\n"

	out, err := applyFeedback([]byte(log), "2026-07-06T13", "good")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count must be preserved: %d", len(lines))
	}
	if !strings.Contains(lines[0], `"future_field":"keep-me"`) {
		t.Fatalf("unmatched lines must stay byte-identical: %s", lines[0])
	}
	if strings.Contains(lines[0], `"quality"`) {
		t.Fatalf("unmatched line must not gain quality: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"quality":"good"`) {
		t.Fatalf("matched line must carry the verdict: %s", lines[1])
	}
}

func TestApplyFeedbackRefusesAmbiguousOrMissing(t *testing.T) {
	log := `{"ts":"2026-07-06T12:00:00Z","lane":"codex"}` + "\n" +
		`{"ts":"2026-07-06T12:30:00Z","lane":"glm"}` + "\n"
	if _, err := applyFeedback([]byte(log), "2026-07-06T12", "bad"); err == nil || !strings.Contains(err.Error(), "2 receipts") {
		t.Fatalf("ambiguous prefix must refuse and count matches: %v", err)
	}
	if _, err := applyFeedback([]byte(log), "2026-07-07", "bad"); err == nil || !strings.Contains(err.Error(), "no receipt") {
		t.Fatalf("missing ts must refuse: %v", err)
	}
	if _, err := applyFeedback([]byte(log), "2026-07-06T12:00", "meh"); err == nil || !strings.Contains(err.Error(), "good|bad") {
		t.Fatalf("verdict is a closed set: %v", err)
	}
}
