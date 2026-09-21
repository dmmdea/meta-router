package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// The quota banner re-rendered byte-identical content on every prompt. Freshness is
// now SEMANTIC: a bucket's ChangedAt (stamped by the ledger only when a displayed
// value moves), a window reset, the GLM latch, or the session's first prompt.

var fnow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// seedBuckets writes the ledger file directly so each test controls ChangedAt
// exactly. The end-to-end test below covers the real write path.
func seedBuckets(t *testing.T, buckets []ledger.Bucket) {
	t.Helper()
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	raw, err := json.Marshal(buckets)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statepaths.Ledger(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func claudeBucket(changed time.Time, resets time.Time) ledger.Bucket {
	return ledger.Bucket{Lane: "claude", Window: ledger.Win5h, UsedPct: 42, ResetsAt: resets,
		Source: "provider", ObservedAt: fnow, ChangedAt: changed}
}

func TestFreshWhenChangedRecently(t *testing.T) {
	seedBuckets(t, []ledger.Bucket{claudeBucket(fnow.Add(-2*time.Minute), fnow.Add(3*time.Hour))})
	h, fresh := quotaHintWithFreshness(fnow)
	if h == "" || !fresh {
		t.Fatalf("a lane changed 2m ago is news: hint=%q fresh=%v", h, fresh)
	}
}

func TestStaleWhenUnchanged(t *testing.T) {
	seedBuckets(t, []ledger.Bucket{claudeBucket(fnow.Add(-40*time.Minute), fnow.Add(3*time.Hour))})
	h, fresh := quotaHintWithFreshness(fnow)
	if h == "" {
		t.Fatal("the hint CONTENT must still be computed; only its freshness is false")
	}
	if fresh {
		t.Fatal("unchanged for 40m: repeating it is a tick, not a delta")
	}
}

// Legacy ledgers carry no ChangedAt. They are stale until something changes;
// the first-prompt rule still shows them once per session.
func TestStaleWhenNeverStamped(t *testing.T) {
	seedBuckets(t, []ledger.Bucket{claudeBucket(time.Time{}, fnow.Add(3*time.Hour))})
	if _, fresh := quotaHintWithFreshness(fnow); fresh {
		t.Fatal("a never-stamped bucket is not news")
	}
}

// Clock skew (a ledger written on a host whose clock runs ahead) must not
// silence the banner forever: a future ChangedAt counts as fresh.
func TestFreshWhenChangedAtIsInTheFuture(t *testing.T) {
	seedBuckets(t, []ledger.Bucket{claudeBucket(fnow.Add(10*time.Minute), fnow.Add(3*time.Hour))})
	if _, fresh := quotaHintWithFreshness(fnow); !fresh {
		t.Fatal("a future ChangedAt must read as fresh, never as silently stale")
	}
}

// A window that just reset changes what the banner shows (a lane recovers) even
// if nothing wrote the ledger since.
func TestFreshWhenAWindowJustReset(t *testing.T) {
	b := claudeBucket(fnow.Add(-3*time.Hour), fnow.Add(-5*time.Minute))
	other := ledger.Bucket{Lane: "codex", Window: ledger.Win5h, UsedPct: 20, ResetsAt: fnow.Add(2 * time.Hour),
		Source: "provider", ObservedAt: fnow, ChangedAt: fnow.Add(-3 * time.Hour)}
	seedBuckets(t, []ledger.Bucket{b, other})
	if _, fresh := quotaHintWithFreshness(fnow); !fresh {
		t.Fatal("a reset 5m ago is news")
	}
}

// Account protection is never gated.
func TestGLMLatchAlwaysFresh(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := os.WriteFile(statepaths.GLMAlert(), []byte(`{"note":"latch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h, fresh := quotaHintWithFreshness(fnow)
	if !strings.Contains(h, "glm HARD-STOP(1313)") || !fresh {
		t.Fatalf("the GLM latch must always render: hint=%q fresh=%v", h, fresh)
	}
}

// --- first-prompt detection -------------------------------------------------

func TestFirstPromptWhenNoTranscript(t *testing.T) {
	if !isFirstPrompt("") {
		t.Fatal("no transcript path: treat as first (shows the banner, the old behaviour)")
	}
	if !isFirstPrompt(filepath.Join(t.TempDir(), "absent.jsonl")) {
		t.Fatal("transcript not written yet: first prompt")
	}
}

func TestFirstPromptWhenOnlyUserRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o644)
	if !isFirstPrompt(p) {
		t.Fatal("no assistant record yet: first prompt")
	}
}

// Measured on real transcripts: the first assistant record sits 200-270 KB in,
// behind the first turn's hook attachments. A fixed-prefix check would misread
// long sessions as first prompts.
func TestNotFirstPromptWhenAssistantRecordIsDeep(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	pad := `{"type":"attachment","content":"` + strings.Repeat("x", 300_000) + `"}` + "\n"
	os.WriteFile(p, []byte(pad+`{"type":"assistant","message":{}}`+"\n"), 0o644)
	if isFirstPrompt(p) {
		t.Fatal("an assistant record 300 KB in means this is not the first prompt")
	}
}

// A match split across the scanner's read boundary must still be found.
func TestNotFirstPromptAcrossChunkBoundary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	marker := `"type":"assistant"`
	head := strings.Repeat("y", firstPromptChunk-len(marker)/2)
	os.WriteFile(p, []byte(head+marker+"\n"), 0o644)
	if isFirstPrompt(p) {
		t.Fatal("the marker straddled a chunk boundary and was missed")
	}
}

// --- end to end through the REAL write path -----------------------------------

// The review of the first attempt: its tests set mtime with os.Chtimes and never
// exercised the unconditional Save() that `route` performs on every consult. This
// drives ledger.Update twice with the SAME value and asserts the banner goes quiet.
func TestRepeatedObservationThroughRealWritePathGoesQuiet(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	// A provider reports a STABLE reset moment; recomputing it per call would move
	// it by nanoseconds, which the ledger correctly treats as a real change.
	resets := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	obs := func() error {
		return ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
			l.ObserveProvider("claude", ledger.Win5h, 42, resets, time.Now())
		})
	}
	if err := obs(); err != nil {
		t.Fatal(err)
	}
	if _, fresh := quotaHintWithFreshness(time.Now()); !fresh {
		t.Fatal("a brand-new observation is news")
	}
	if err := obs(); err != nil { // route re-consults: same number, file rewritten
		t.Fatal(err)
	}
	l, _ := ledger.OpenChecked(statepaths.Ledger())
	b, _ := l.Bucket("claude", ledger.Win5h)
	if _, fresh := quotaHintWithFreshness(b.ChangedAt.Add(20 * time.Minute)); fresh {
		t.Fatal("20m after the last REAL change, a re-observed identical value must not read as fresh")
	}
}

// TestQuotaBannerGateWiredE2E drives the BUILT binary, so it fails if the gate in
// main.go is removed or bypassed -- the unit tests above would all still pass.
// Same stale ledger, two transcripts: an established session gets no banner; a
// session with no assistant turn yet gets it once.
func TestQuotaBannerGateWiredE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	stateDir := filepath.Join(work, "orchstate")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-40 * time.Minute)
	raw, _ := json.Marshal([]ledger.Bucket{{Lane: "claude", Window: ledger.Win5h, UsedPct: 42,
		ResetsAt: time.Now().Add(3 * time.Hour), Source: "provider", ObservedAt: time.Now(), ChangedAt: stale}})
	if err := os.WriteFile(filepath.Join(stateDir, "ledger.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	idxPath := writeIndex(t, work, "embeddinggemma/tpl99", "tpl99")

	established := filepath.Join(work, "established.jsonl")
	os.WriteFile(established, []byte(`{"type":"user"}`+"\n"+`{"type":"assistant","message":{}}`+"\n"), 0o644)

	run := func(transcript string) string {
		cmd := exec.Command(bin, "-index", idxPath, "-log", filepath.Join(work, "usage.jsonl"),
			"-endpoint", "http://127.0.0.1:1", "-timeout-ms", "5000")
		in, _ := json.Marshal(map[string]string{"prompt": "QA test a web application for me please",
			"transcript_path": transcript})
		cmd.Stdin = bytes.NewReader(in)
		cmd.Env = append(os.Environ(), "MR_ORCH_STATE="+stateDir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("mr-hook exited non-zero: %v", err)
		}
		return string(out)
	}
	if out := run(established); strings.Contains(out, "mr-orchestrate quota") {
		t.Fatalf("established session + stale ledger must NOT repeat the banner: %q", out)
	}
	if out := run(filepath.Join(work, "not-yet-written.jsonl")); !strings.Contains(out, "mr-orchestrate quota") {
		t.Fatalf("first prompt must show the banner once: %q", out)
	}
}

// Review finding (HIGH, read side): a stamp far in the future is a clock error.
// It must not pin the banner on; the first-prompt rule still shows it per session.
func TestFarFutureChangedAtIsNotTrusted(t *testing.T) {
	seedBuckets(t, []ledger.Bucket{claudeBucket(fnow.Add(48*time.Hour), fnow.Add(72*time.Hour))})
	if _, fresh := quotaHintWithFreshness(fnow); fresh {
		t.Fatal("a ChangedAt two days ahead must not read as fresh forever")
	}
}

// Review finding (MEDIUM): a transcript writer that emits spaced JSON must not turn
// every prompt into a "first" prompt.
func TestNotFirstPromptWithSpacedMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(p, []byte(`{"type": "user"}`+"\n"+`{"type" : "assistant", "message": {}}`+"\n"), 0o644)
	if isFirstPrompt(p) {
		t.Fatal(`"type" : "assistant" (spaced) was not recognised`)
	}
}
