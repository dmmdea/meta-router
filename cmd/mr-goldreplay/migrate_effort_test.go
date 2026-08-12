package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/policyeval"
)

// The real legacy rows carry NO `effort` key at all (the Row struct had no such
// field when they were written). These fixtures are that exact shape.
const (
	legacyRow  = `{"ts":"2026-07-19T05:32:44Z","task":"AC-10","class":"agentic-coding","lane":"local","model":"gemma4-cascade","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":3804,"note":"no diff in output"}`
	pinnedRow  = `{"ts":"t","task":"RS-01","class":"research","lane":"claude","model":"claude-opus-4-8","effort":"xhigh","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}`
	blankRow   = `{"ts":"t","task":"RS-02","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}`
	tornLine   = `not json — a torn line must survive verbatim`
	deferredRw = `{"ts":"t","task":"EX-01","class":"extraction","lane":"glm","model":"glm-5.2","trial":1,"dispatched":false,"outcome_class":"deferred","verifier_pass":false,"latency_ms":1}`
)

func fixture(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func TestMigrateEffortStampsOnlyBlankRows(t *testing.T) {
	in := fixture(legacyRow, pinnedRow, blankRow, deferredRw)
	out, changed, err := migrateEffort(in)
	if err != nil {
		t.Fatalf("migrateEffort: %v", err)
	}
	// legacy (absent key), blank (present-but-empty) and deferred (absent key)
	// all get stamped; only the pinned row is left alone. A deferred row is a
	// HOLE, but it is still a row that must carry a well-formed effort.
	if changed != 3 {
		t.Fatalf("changed = %d, want 3 (two absent-key rows + one blank-value row)", changed)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("line count changed: %d", len(lines))
	}
	if lines[1] != pinnedRow {
		t.Fatalf("a pinned row must be byte-identical:\n got %s\nwant %s", lines[1], pinnedRow)
	}
	for i, ln := range lines {
		var r Row
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("line %d no longer parses as JSON: %v\n%s", i, err, ln)
		}
		if r.Effort == "" {
			t.Fatalf("line %d still has a blank effort: %s", i, ln)
		}
	}
	var stamped Row
	if err := json.Unmarshal([]byte(lines[0]), &stamped); err != nil {
		t.Fatal(err)
	}
	if stamped.Effort != policyeval.EffortUnrecorded {
		t.Fatalf("legacy row effort = %q, want %q", stamped.Effort, policyeval.EffortUnrecorded)
	}
	// Every other field must survive the stamp untouched.
	if stamped.Task != "AC-10" || stamped.Model != "gemma4-cascade" || stamped.Note != "no diff in output" ||
		stamped.LatencyMs != 3804 || !stamped.Dispatched {
		t.Fatalf("the stamp altered another field: %+v", stamped)
	}
}

// A second pass must report nothing changed AND produce byte-identical output:
// the migration is run against the live oracle, and a non-fixed-point rewrite
// would churn the file (and its git history) on every run.
func TestMigrateEffortIsIdempotent(t *testing.T) {
	in := fixture(legacyRow, pinnedRow, blankRow, deferredRw)
	first, changed1, err := migrateEffort(in)
	if err != nil || changed1 == 0 {
		t.Fatalf("first pass: changed=%d err=%v", changed1, err)
	}
	second, changed2, err := migrateEffort(first)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if changed2 != 0 {
		t.Fatalf("second pass changed %d rows; the migration is not a fixed point", changed2)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("second pass is not byte-identical:\n%s\n---\n%s", first, second)
	}
}

// A torn line survives verbatim: loadDone already tolerates one, and silently
// dropping or rewriting bytes we cannot parse is how a migration eats evidence.
func TestMigrateEffortPreservesUnparseableLines(t *testing.T) {
	in := fixture(tornLine, legacyRow)
	out, changed, err := migrateEffort(in)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (the torn line is not a row)", changed)
	}
	if !strings.HasPrefix(string(out), tornLine+"\n") {
		t.Fatalf("torn line not preserved verbatim: %q", string(out))
	}
}

// CRLF must survive: the oracle is written and read on Windows.
func TestMigrateEffortPreservesCRLF(t *testing.T) {
	in := []byte(legacyRow + "\r\n" + pinnedRow + "\r\n")
	out, changed, err := migrateEffort(in)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if bytes.Count(out, []byte("\r\n")) != 2 {
		t.Fatalf("CRLF line endings not preserved: %q", string(out))
	}
	var r Row
	if err := json.Unmarshal(bytes.TrimRight(bytes.Split(out, []byte("\r\n"))[0], "\r"), &r); err != nil {
		t.Fatalf("stamped CRLF line does not parse: %v", err)
	}
	if r.Effort != policyeval.EffortUnrecorded {
		t.Fatalf("effort = %q", r.Effort)
	}
}

// The file mode writes <path>.tmp and renames — never an in-place truncation,
// which loses the whole oracle if the process dies mid-write.
func TestMigrateEffortFileRenamesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "oracle.jsonl")
	if err := os.WriteFile(p, fixture(legacyRow, pinnedRow), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := migrateEffortFile(p)
	if err != nil {
		t.Fatalf("migrateEffortFile: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temp file must not survive a successful migration")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"effort":"`+policyeval.EffortUnrecorded+`"`) {
		t.Fatalf("migrated file lacks the stamp:\n%s", b)
	}
	// Second run over the SAME file: nothing to do, bytes unchanged.
	again, err := migrateEffortFile(p)
	if err != nil || again != 0 {
		t.Fatalf("second file pass: changed=%d err=%v", again, err)
	}
	b2, _ := os.ReadFile(p)
	if !bytes.Equal(b, b2) {
		t.Fatal("second file pass rewrote bytes")
	}
}

func TestMigrateEffortFileMissingIsAnError(t *testing.T) {
	if _, err := migrateEffortFile(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Fatal("a missing oracle must be a loud error, not a silent zero-row success")
	}
}

// ── the VERBATIM promise (review 2026-08-12, round 3) ───────────────────────

// A valid-JSON line with no task+lane identity is a FOREIGN record (a quota or
// dispatch row from a neighboring JSONL). looksLikeOracle warns those are
// "passed through VERBATIM, not stamped" — and stampLine previously stamped
// them anyway. Under the 50% refusal threshold, a mixed file had its foreign
// records rewritten while the run reported the opposite.
func TestMigrateEffortPassesForeignRecordsVerbatim(t *testing.T) {
	foreignA := `{"ts":"t","lane":"claude","tokens":1234,"usd":0.42,"window":"5h"}`
	foreignB := `{"some":"unrelated record","seq":1}`
	in := fixture(legacyRow, foreignA, pinnedRow, foreignB)
	out, changed, err := migrateEffort(in)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (only the legacy ORACLE row)", changed)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if lines[1] != foreignA {
		t.Fatalf("foreign record was mutated:\n got %s\nwant %s", lines[1], foreignA)
	}
	if lines[3] != foreignB {
		t.Fatalf("foreign record was mutated:\n got %s\nwant %s", lines[3], foreignB)
	}
	if strings.Contains(lines[1]+lines[3], "effort") {
		t.Fatal("a foreign record must never gain an effort key")
	}
}

// looksLikeOracle and stampLine must share ONE identity predicate — two
// predicates for "is this line an oracle row" is exactly how the warning and
// the behavior disagreed.
func TestHasOracleIdentityAgreesWithLooksLikeOracle(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{legacyRow, true},
		{pinnedRow, true},
		{`{"ts":"t","lane":"claude","tokens":1}`, false}, // no task
		{`{"task":"T-01","tokens":1}`, false},            // no lane
		{`not json`, false},
		{`{"task":"","lane":""}`, false},
	}
	for _, c := range cases {
		if got := hasOracleIdentity([]byte(c.line)); got != c.want {
			t.Fatalf("hasOracleIdentity(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}
