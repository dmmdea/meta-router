package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/policyeval"
)

// The live shapes, one per signature, across the lanes and BOTH evidence
// classes the defect landed on (four of the copilot rows are
// dispatched-not-ok, and that is evidence).
const (
	qCodexHeader   = `{"ts":"t","task":"AC-10","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | error: git diff header lacks filename information when removing 1 leading pathname component at .candidate.diff:70"}`
	qClaudeCorrupt = `{"ts":"t","task":"AC-02","class":"agentic-coding","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | error: corrupt patch at .candidate.diff:161"}`
	qGlmRecount    = `{"ts":"t","task":"AC-05","class":"agentic-coding","lane":"glm","model":"glm-5.2","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | warning: recount: unexpected line: ntIndent is the indent | error: corrupt patch"}`
	qCopilotNoPatch = `{"ts":"t","task":"AC-03","class":"agentic-coding","lane":"copilot","model":"auto","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"dispatched-not-ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | error: No valid patches in input (allow with \"--allow-empty\"); served=gpt-5.6-luna"}`
	qLunaWarning   = `{"ts":"t","task":"QE-03","class":"quick-edit","lane":"codex","model":"gpt-5.6-luna","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | warning: recount: unexpected line: warning: in the working copy of '.gocache/02/x', LF will be replaced by CRLF","extra_unknown":{"k":[1,2]}}`
	// Not selected:
	qGenuineFail  = `{"ts":"t","task":"AC-07","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: go test: exit status 1 | --- FAIL: TestParseSkillMD_BlockFolded"}`
	qPass         = `{"ts":"t","task":"AC-04","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}`
	qHoleDeferred = `{"ts":"t","task":"AC-12","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":false,"outcome_class":"deferred","verifier_pass":false,"latency_ms":9,"note":"corrupt patch at x (but a hole already)"}`
	qHoleVerifyErr = `{"ts":"t","task":"AC-13","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"verify_error","verifier_pass":false,"latency_ms":9,"note":"goldverify: corrupt patch at x"}`
	qAlready      = `{"ts":"t","task":"AC-14","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: corrupt patch at y","quarantined":"v0.40.4","quarantine_reason":"r"}`
	qResearchPass = `{"ts":"t","task":"RS-01","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}`
	// The autocrlf shape ALONE — the only carrier of `warning: in the working
	// copy`. Without it that signature is redundant with `recount:` on
	// qLunaWarning and deleting it from the union stays green.
	qWorkingCopyOnly = `{"ts":"t","task":"AC-08","class":"agentic-coding","lane":"copilot","model":"auto","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | warning: in the working copy of 'internal/x/y.go', LF will be replaced by CRLF"}`
	// An UNTOUCHED lane: evidence, no signature, so `local` must never appear
	// in the pass-rate table (Before == After ⇒ the render skips the line).
	qUntouchedLane = `{"ts":"t","task":"QE-09","class":"quick-edit","lane":"local","model":"gemma4-cascade","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9,"note":"verify-fail: go test: exit status 1 | ok"}`
	// STAGE BOUND: a genuine `go test` failure whose captured output happens
	// to print a signature string. The goldset is self-hosting, so this is
	// the real hazard: without the apply-stage gate this row is laundered
	// from a measured model failure into a hole.
	qTestStageSignature = `{"ts":"t","task":"AC-11","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: go test: exit status 1 | --- FAIL: TestPatchGrammar | want error \"corrupt patch at\", got nil"}`
	// PROVENANCE BOUND: written under v0.40.4+ (it carries diff_source), so
	// its printed patch failing to apply is the MODEL's failure by the
	// anti-laundering rule — never a harness fault.
	qPostFixPrinted = `{"ts":"t","task":"AC-15","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":9,"note":"verify-fail: git apply: exit status 128 | error: corrupt patch at .candidate.diff:12","diff_source":"printed-decoded"}`
)

func qFixture() []byte {
	return fixture(qCodexHeader, qClaudeCorrupt, qGlmRecount, qCopilotNoPatch, qLunaWarning,
		qGenuineFail, qPass, qHoleDeferred, qHoleVerifyErr, qAlready, qResearchPass, tornLine,
		qWorkingCopyOnly, qUntouchedLane, qTestStageSignature, qPostFixPrinted)
}

// The selector is the UNION, applied to every evidence-bearing class, and
// only to evidence: holes and already-quarantined rows are left alone.
func TestQuarantineSelectorUnionAcrossLanesAndClasses(t *testing.T) {
	rep := quarantineSelect(qFixture())
	if len(rep.Matched) != 6 {
		t.Fatalf("matched %d, want 6: %+v", len(rep.Matched), rep.Matched)
	}
	want := map[string]map[string]int{
		"codex/gpt-6-astra":      {"ok": 1},
		"claude/claude-sonnet-5": {"ok": 1},
		"glm/glm-5.2":            {"ok": 1},
		"copilot/auto":           {"dispatched-not-ok": 1, "ok": 1},
		"codex/gpt-5.6-luna":     {"ok": 1},
	}
	for lm, cls := range want {
		for c, n := range cls {
			if rep.ByLaneModel[lm][c] != n {
				t.Fatalf("%s %s = %d, want %d (%+v)", lm, c, rep.ByLaneModel[lm][c], n, rep.ByLaneModel)
			}
		}
	}
	for _, m := range rep.Matched {
		switch m.Row.Task {
		case "AC-07", "AC-04", "AC-12", "AC-13", "AC-14", "RS-01", "QE-09", "AC-11", "AC-15":
			t.Fatalf("%s must not be selected: %+v", m.Row.Task, m.Row)
		}
	}
	// The two bounds are COUNTED, not silently dropped: AC-11 carries a
	// signature at the `go test` stage, AC-15 carries one at the apply stage
	// but was written under v0.40.4+.
	if rep.ExcludedStage != 1 || rep.ExcludedPostFix != 1 {
		t.Fatalf("bounds must be counted: stage=%d postfix=%d", rep.ExcludedStage, rep.ExcludedPostFix)
	}
	// Both bounded rows stay EVIDENCE (they are not quarantined), so they
	// must still be in the After denominator.
	if rep.After["codex"].N != 4 {
		t.Fatalf("bounded rows must remain evidence after the quarantine: %+v", rep.After["codex"])
	}
	// Before/after: codex evidence rows = header, luna, genuine, pass,
	// test-stage-signature, post-fix-printed (6; 1 pass) → after drops the
	// header and luna (4; 1 pass). claude: corrupt + research (2; 1) → research (1; 1).
	if rep.Before["codex"] != (laneStat{N: 6, Pass: 1}) || rep.After["codex"] != (laneStat{N: 4, Pass: 1}) {
		t.Fatalf("codex before/after: %+v / %+v", rep.Before["codex"], rep.After["codex"])
	}
	if rep.Before["claude"] != (laneStat{N: 2, Pass: 1}) || rep.After["claude"] != (laneStat{N: 1, Pass: 1}) {
		t.Fatalf("claude before/after: %+v / %+v", rep.Before["claude"], rep.After["claude"])
	}
	// glm loses its only evidence row: After must still carry the lane at n=0.
	if rep.After["glm"] != (laneStat{}) {
		t.Fatalf("glm after: %+v", rep.After["glm"])
	}
	if rep.BeforeAgentic["codex"] != (laneStat{N: 5, Pass: 1}) || rep.AfterAgentic["codex"] != (laneStat{N: 4, Pass: 1}) {
		t.Fatalf("codex agentic before/after: %+v / %+v", rep.BeforeAgentic["codex"], rep.AfterAgentic["codex"])
	}
	// The untouched lane is REAL evidence in the fixture and must be
	// identical on both sides — the render's skip is only meaningful then.
	if rep.Before["local"] != (laneStat{N: 1, Pass: 1}) || rep.After["local"] != rep.Before["local"] {
		t.Fatalf("untouched lane before/after: %+v / %+v", rep.Before["local"], rep.After["local"])
	}
	out := rep.render("v0.40.4", qDryRun)
	for _, s := range []string{"DRY RUN", "6 evidence row(s)", "copilot/auto", "dispatched-not-ok:1", "codex     0.17 (n=6) → 0.25 (n=4)", "receipt:", "paid one on copilot", "boundary: 2 signature-carrying row(s) NOT selected"} {
		if !strings.Contains(out, s) {
			t.Fatalf("render missing %q:\n%s", s, out)
		}
	}
	// A lane the quarantine does not touch gets no pass-rate line.
	if strings.Contains(out, "\n  local ") {
		t.Fatalf("untouched lane must not be listed:\n%s", out)
	}
	// An ABORTED apply must never read like a completed one.
	ab := rep.render("v0.40.4", qAborted)
	if !strings.Contains(ab, "APPLY ABORTED") || !strings.Contains(ab, "the oracle is unchanged") || strings.Contains(ab, "receipt:") {
		t.Fatalf("aborted render must say nothing was written and drop the applied-receipt:\n%s", ab)
	}
}

// A dry run touches nothing — bytes, mtime, no temp, no backup.
func TestRequarantineDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "oracle.jsonl")
	in := qFixture()
	if err := os.WriteFile(p, in, 0o644); err != nil {
		t.Fatal(err)
	}
	st0, _ := os.Stat(p)
	rep, _, err := requarantineFile(p, "v0.40.4", false)
	if err != nil || len(rep.Matched) != 6 {
		t.Fatalf("dry run: %v, matched %d", err, len(rep.Matched))
	}
	b, _ := os.ReadFile(p)
	st1, _ := os.Stat(p)
	if string(b) != string(in) || !st1.ModTime().Equal(st0.ModTime()) {
		t.Fatal("dry run must not write")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "oracle.jsonl.*"))
	if len(m) != 0 {
		t.Fatalf("dry run left files: %v", m)
	}
}

// -apply refuses an oracle that is not committed clean, then stamps by byte
// splice (unknown fields, CR terminators and torn lines survive), writes a
// backup, and is a fixed point.
func TestRequarantineApplyRequiresCommittedOracleAndIsAFixedPoint(t *testing.T) {
	dir, _ := newTempRepo(t)
	p := filepath.Join(dir, "oracle.jsonl")
	in := []byte(strings.ReplaceAll(string(qFixture()), "\n", "\r\n"))
	if err := os.WriteFile(p, in, 0o644); err != nil {
		t.Fatal(err)
	}
	// untracked → refuse, nothing written
	if _, _, err := requarantineFile(p, "v0.40.4", true); err == nil || !strings.Contains(err.Error(), "commit the oracle first") {
		t.Fatalf("untracked oracle must refuse -apply: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(in) {
		t.Fatal("refused apply must not write")
	}
	gitRun(t, dir, "add", "oracle.jsonl")
	// tracked but dirty (staged, uncommitted) → refuse
	if _, _, err := requarantineFile(p, "v0.40.4", true); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty oracle must refuse -apply: %v", err)
	}
	gitRun(t, dir, "commit", "-q", "-m", "oracle")
	// outside a repo entirely → refuse
	loose := filepath.Join(t.TempDir(), "oracle.jsonl")
	if err := os.WriteFile(loose, in, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := requarantineFile(loose, "v0.40.4", true); err == nil || !strings.Contains(err.Error(), "not tracked") {
		t.Fatalf("oracle outside git must refuse -apply: %v", err)
	}

	rep, bakPath, err := requarantineFile(p, "v0.40.4", true)
	if err != nil || len(rep.Matched) != 6 {
		t.Fatalf("apply: %v, matched %d", err, len(rep.Matched))
	}
	out, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\r\n")
	if len(lines) != 16 {
		t.Fatalf("line count changed: %d\n%s", len(lines), out)
	}
	if strings.Count(string(out), `"quarantined":"v0.40.4"`) != 7 { // 6 stamped + the pre-existing one
		t.Fatalf("stamp count wrong:\n%s", out)
	}
	// The bounded rows are byte-identical: neither bound may stamp.
	if !strings.Contains(string(out), qTestStageSignature) || !strings.Contains(string(out), qPostFixPrinted) {
		t.Fatalf("bounded rows must survive verbatim:\n%s", out)
	}
	if !strings.Contains(lines[4], `"extra_unknown":{"k":[1,2]}`) || !strings.HasSuffix(lines[4], `"quarantine_reason":"`+quarantineReason+`"}`) {
		t.Fatalf("splice must keep unknown fields and append the members: %s", lines[4])
	}
	if lines[11] != tornLine || lines[6] != qPass {
		t.Fatal("untouched lines must be verbatim")
	}
	if !strings.Contains(string(out), "\r\n") {
		t.Fatal("CRLF terminators must survive")
	}
	bak, _ := filepath.Glob(filepath.Join(dir, "oracle.jsonl.bak-requarantine-*"))
	if len(bak) != 1 {
		t.Fatalf("one backup expected: %v", bak)
	}
	if bakPath != bak[0] {
		t.Fatalf("the applied backup path must be returned for the receipt: %q vs %q", bakPath, bak[0])
	}
	if bb, _ := os.ReadFile(bak[0]); string(bb) != string(in) {
		t.Fatal("backup must be the pre-state")
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	// Fixed point: the stamped rows are holes now, nothing matches, nothing moves.
	gitRun(t, dir, "add", "oracle.jsonl")
	gitRun(t, dir, "commit", "-q", "-m", "stamped")
	st0, _ := os.Stat(p)
	rep2, bak2, err := requarantineFile(p, "v0.40.4", true)
	if err != nil || len(rep2.Matched) != 0 || bak2 != "" {
		t.Fatalf("second pass: %v, matched %d", err, len(rep2.Matched))
	}
	st1, _ := os.Stat(p)
	if !st1.ModTime().Equal(st0.ModTime()) {
		t.Fatal("fixed point must not touch the file")
	}
	// Every stamped row now reads as a hole through the shared predicate.
	for _, m := range rep.Matched {
		r := m.Row
		r.Quarantined = "v0.40.4"
		if rowIsEvidence(r) || policyeval.IsEvidence(r.Dispatched, r.OutcomeClass, r.Quarantined) {
			t.Fatalf("quarantined row still evidence: %+v", r)
		}
	}
}


// The concurrent-writer guard, made observable: a sweep appends a row after
// the bytes were read, and commits it so the git gate is not what fires. The
// apply must ABORT and the appended row must SURVIVE. Deleting the
// pre-rename byte comparison makes this test lose the row, which is the
// whole failure mode: the rename would publish a rewrite built from bytes
// that are no longer what the file holds.
func TestRequarantineAbortsWhenTheOracleGrowsUnderIt(t *testing.T) {
	dir, _ := newTempRepo(t)
	p := filepath.Join(dir, "oracle.jsonl")
	in := qFixture()
	if err := os.WriteFile(p, in, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "oracle.jsonl")
	gitRun(t, dir, "commit", "-q", "-m", "oracle")
	appended := qPass[:len(qPass)-1] + `,"task_note":"appended by a live sweep"}`
	fired := false
	requarantineAfterRead = func() {
		if fired {
			return
		}
		fired = true
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		// A distinct mtime as well as a distinct size: some filesystems
		// report a coarse mtime, so the guard must not depend on either alone.
		if _, err := f.WriteString(appended + "\n"); err != nil {
			t.Error(err)
		}
		f.Close()
		// Commit it: otherwise oracleCommitted refuses first and this test
		// would prove the git gate, not the pre-rename comparison.
		gitRun(t, dir, "add", "oracle.jsonl")
		gitRun(t, dir, "commit", "-q", "-m", "sweep append")
	}
	defer func() { requarantineAfterRead = nil }()

	rep, bak, err := requarantineFile(p, "v0.40.4", true)
	if err == nil {
		t.Fatalf("apply must abort when the oracle grows under it (matched %d)", len(rep.Matched))
	}
	if !strings.Contains(err.Error(), "changed while the quarantine was being prepared") {
		t.Fatalf("wrong refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "another process is writing it") {
		t.Fatalf("the refusal must name the cause: %v", err)
	}
	if bak != "" {
		t.Fatalf("an aborted apply must report no backup, got %q", bak)
	}
	after, _ := os.ReadFile(p)
	if !strings.Contains(string(after), "appended by a live sweep") {
		t.Fatal("THE APPENDED ROW WAS LOST: the rename went ahead on bytes read before the append")
	}
	// Exactly the ONE stamp the fixture already carried (qAlready): the
	// aborted apply added none.
	if n := strings.Count(string(after), `"quarantined":"v0.40.4"`); n != 1 {
		t.Fatalf("an aborted apply must not stamp anything, found %d stamps", n)
	}
	if b, _ := filepath.Glob(filepath.Join(dir, "oracle.jsonl.bak-requarantine-*")); len(b) != 0 {
		t.Fatalf("an aborted apply must leave no backup: %v", b)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("an aborted apply must leave no temp file")
	}
}
// A quarantined row leaves the resume set (the next sweep refills it) but
// KEEPS its identity in the drift indexes: refilling astra at the astra pin
// is a re-measurement, not a re-key, so the model tier must not fire.
// Mutation: indexing only evidence rows in loadDone makes this red.
func TestQuarantinedRowIsHoleButKeepsIdentity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	rows := fixture(
		`{"task":"AC-01","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"note":"verify-fail: git apply: corrupt patch at x","quarantined":"v0.40.4"}`,
		`{"task":"AC-01","class":"agentic-coding","lane":"codex","model":"gpt-5.6-terra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}`,
	)
	if err := os.WriteFile(p, rows, 0o644); err != nil {
		t.Fatal(err)
	}
	rs := loadDone(p)
	if rs.done[rowKey("AC-01", "codex", "gpt-6-astra", policyeval.EffortUnrecorded, 1)] {
		t.Fatal("quarantined row must not be in the resume set")
	}
	if !rs.done[rowKey("AC-01", "codex", "gpt-5.6-terra", policyeval.EffortUnrecorded, 1)] {
		t.Fatal("the evidence row must be in the resume set")
	}
	models := rs.modelsByIdent[identKey("AC-01", "codex", 1)]
	if !models["gpt-6-astra"] || !models["gpt-5.6-terra"] {
		t.Fatalf("quarantined row must keep its model in the identity index: %v", models)
	}
	astra := policyeval.Config{Lane: "codex", Model: "gpt-6-astra", Effort: policyeval.EffortUnrecorded}
	d := detectDrift([]plannedCell{{Task: "AC-01", Config: astra, Trial: 1}}, rs)
	if d.any() {
		t.Fatalf("refilling a quarantined cell at its own pin must not read as drift: %+v", d)
	}
	// A different pin on the same ident still drifts — the guard is intact.
	sol := policyeval.Config{Lane: "codex", Model: "gpt-5.6-sol", Effort: policyeval.EffortUnrecorded}
	if d := detectDrift([]plannedCell{{Task: "AC-01", Config: sol, Trial: 1}}, rs); !d.any() {
		t.Fatal("a new model pin on a recorded ident must still drift")
	}
}

// End to end on the built binary: a sweep launched after a quarantine
// refills the quarantined cell (1 run now) and does NOT die at the drift
// guard — the failure mode that made the three-signature proposal unsafe.
func TestIntegrationQuarantinedCellRefillsWithoutDrift(t *testing.T) {
	rows := `{"task":"T-01","class":"research","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"note":"verify-fail: git apply: corrupt patch at x","quarantined":"v0.40.4"}
{"task":"T-01","class":"research","lane":"codex","model":"gpt-5.6-terra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-02","class":"research","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`
	dir, goldset, oracle := intFixture(t, rows)
	code, so, se := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-lanes", "codex",
		"-codex-model", "gpt-6-astra", "-codex-effort", "unrecorded",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 {
		t.Fatalf("exit = %d (a quarantine must not trip the drift guard)\nstderr: %s", code, se)
	}
	if strings.Contains(se, "MODEL DRIFT") {
		t.Fatalf("drift guard fired on a quarantined refill: %s", se)
	}
	if !strings.Contains(so, "(1 run now, 1 already recorded)") {
		t.Fatalf("the quarantined cell must be the one refilled: %s", so)
	}
}

// The dry run through the built binary prints the plan and exits 0 without
// touching the file; -apply on an uncommitted file refuses with exit 2.
func TestIntegrationRequarantineDryRunAndRefusal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "oracle.jsonl")
	in := qFixture()
	if err := os.WriteFile(p, in, 0o644); err != nil {
		t.Fatal(err)
	}
	code, so, se := intRun(t, dir, "-requarantine", p)
	if code != 0 || !strings.Contains(so, "DRY RUN") || !strings.Contains(so, "6 evidence row(s)") {
		t.Fatalf("dry run: exit %d\n%s\n%s", code, so, se)
	}
	// The boundary is on the operator's screen, including the torn line.
	if !strings.Contains(so, "boundary: 2 signature-carrying row(s) NOT selected") || !strings.Contains(so, "1 line(s) undecodable") {
		t.Fatalf("dry run must print the boundary:\n%s", so)
	}
	if b, _ := os.ReadFile(p); string(b) != string(in) {
		t.Fatal("dry run wrote the oracle")
	}
	code, so, se = intRun(t, dir, "-requarantine", p, "-apply")
	if code != 2 || !strings.Contains(se, "commit the oracle first") {
		t.Fatalf("apply on an untracked oracle must refuse: exit %d\n%s\n%s", code, so, se)
	}
	// A refused apply must READ as refused: aborted header, no applied
	// receipt, and an error that says nothing was written.
	if !strings.Contains(so, "APPLY ABORTED") || strings.Contains(so, "receipt:") || !strings.Contains(se, "NOT applied, the oracle is unchanged") {
		t.Fatalf("a refused apply must not read like a completed one:\nSTDOUT %s\nSTDERR %s", so, se)
	}
	if b, _ := os.ReadFile(p); string(b) != string(in) {
		t.Fatal("refused apply wrote the oracle")
	}
	// -apply without -requarantine is a typo, not a silent no-op.
	if code, _, se := intRun(t, dir, "-apply"); code != 2 || !strings.Contains(se, "-apply has no effect on its own") {
		t.Fatalf("bare -apply must refuse: exit %d\n%s", code, se)
	}
	// A backup is written ONLY by a completed apply: the refusals left none.
	if bak, _ := filepath.Glob(filepath.Join(dir, "oracle.jsonl.bak-requarantine-*")); len(bak) != 0 {
		t.Fatalf("a refused apply must leave no backup: %v", bak)
	}
}
