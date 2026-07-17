package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RS8 codex leg: JSONL variant of verifySchema — key paths are prefixed by
// event type, so a live capture missing usage under turn.completed is the
// breaking class.
func TestVerifyCodexKeyPathsPerEventType(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "codex", "exec-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := jsonlKeys(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"thread.started.thread_id",
		"item.completed.item.text",
		"turn.completed.usage.input_tokens",
		"turn.completed.usage.cached_input_tokens",
	} {
		if !keys[want] {
			t.Fatalf("fixture key paths missing %q: %v", want, keys)
		}
	}

	// Identical capture → stable, not breaking.
	if report, breaking, err := verifyCodexSchema(fixture, fixture); err != nil || breaking {
		t.Fatalf("self-diff must be stable: breaking=%v err=%v\n%s", breaking, err, report)
	}

	// A live capture whose turn.completed lost usage → REMOVED keys → breaking.
	live := strings.ReplaceAll(string(fixture),
		`{"type":"turn.completed","usage":{"input_tokens":18487,"cached_input_tokens":18176,"output_tokens":5,"reasoning_output_tokens":0}}`,
		`{"type":"turn.completed"}`)
	report, breaking, err := verifyCodexSchema(fixture, []byte(live))
	if err != nil || !breaking {
		t.Fatalf("missing usage under turn.completed must be breaking: breaking=%v err=%v\n%s", breaking, err, report)
	}
	if !strings.Contains(report, "turn.completed.usage.input_tokens") {
		t.Fatalf("report must name the removed path:\n%s", report)
	}
}

// The codex burn-anomaly alert LATCHES: the first violation's timestamp is
// the evidence — later anomalies never overwrite it; probe --ack-codex clears.
func TestCodexAlertLatchIsSticky(t *testing.T) {
	p := filepath.Join(t.TempDir(), "codex-alert.json")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if err := writeCodexAlert(p, "first misfire", t0); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexAlert(p, "second misfire", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "first misfire") || strings.Contains(string(b), "second misfire") {
		t.Fatalf("latch must keep the FIRST violation: %s", b)
	}
}
