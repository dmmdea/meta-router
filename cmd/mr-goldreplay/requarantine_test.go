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
)

func qFixture() []byte {
	return fixture(qCodexHeader, qClaudeCorrupt, qGlmRecount, qCopilotNoPatch, qLunaWarning,
		qGenuineFail, qPass, qHoleDeferred, qHoleVerifyErr, qAlready, qResearchPass, tornLine)
}

// The selector is the UNION, applied to every evidence-bearing class, and
// only to evidence: holes and already-quarantined rows are left alone.
func TestQuarantineSelectorUnionAcrossLanesAndClasses(t *testing.T) {
	rep := quarantineSelect(qFixture())
	if len(rep.Matched) != 5 {
		t.Fatalf("matched %d, want 5: %+v", len(rep.Matched), rep.Matched)
	}
	want := map[string]map[string]int{
		"codex/gpt-6-astra":      {"ok": 1},
		"claude/claude-sonnet-5": {"ok": 1},
		"glm/glm-5.2":            {"ok": 1},
		"copilot/auto":           {"dispatched-not-ok": 1},
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
		case "AC-07", "AC-04", "AC-12", "AC-13", "AC-14", "RS-01":
			t.Fatalf("%s must not be selected: %+v", m.Row.Task, m.Row)
		}
	}
	// Before/after: codex evidence rows = header, luna, genuine, pass (4; 1 pass)
	// → after = genuine, pass (2; 1 pass). claude: corrupt + research (2; 1) → research (1; 1).
	if rep.Before["codex"] != (laneStat{N: 4, Pass: 1}) || rep.After["codex"] != (laneStat{N: 2, Pass: 1}) {
		t.Fatalf("codex before/after: %+v / %+v", rep.Before["codex"], rep.After["codex"])
	}
	if rep.Before["claude"] != (laneStat{N: 2, Pass: 1}) || rep.After["claude"] != (laneStat{N: 1, Pass: 1}) {
		t.Fatalf("claude before/after: %+v / %+v", rep.Before["claude"], rep.After["claude"])
	}
	// glm loses its only evidence row: After must still carry the lane at n=0.
	if rep.After["glm"] != (laneStat{}) {
		t.Fatalf("glm after: %+v", rep.After["glm"])
	}
	if rep.BeforeAgentic["codex"] != (laneStat{N: 3, Pass: 1}) || rep.AfterAgentic["codex"] != (laneStat{N: 2, Pass: 1}) {
		t.Fatalf("codex agentic before/after: %+v / %+v", rep.BeforeAgentic["codex"], rep.AfterAgentic["codex"])
	}
	out := rep.render("v0.40.4", false)
	for _, s := range []string{"DRY RUN", "5 evidence row(s)", "copilot/auto", "dispatched-not-ok:1", "codex     0.25 (n=4) → 0.50 (n=2)", "receipt:", "paid one on copilot"} {
		if !strings.Contains(out, s) {
			t.Fatalf("render missing %q:\n%s", s, out)
		}
	}
	// A lane the quarantine does not touch gets no pass-rate line.
	if strings.Contains(out, "\n  local ") {
		t.Fatalf("untouched lane must not be listed:\n%s", out)
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
	rep, err := requarantineFile(p, "v0.40.4", false)
	if err != nil || len(rep.Matched) != 5 {
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
	if _, err := requarantineFile(p, "v0.40.4", true); err == nil || !strings.Contains(err.Error(), "commit the oracle first") {
		t.Fatalf("untracked oracle must refuse -apply: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(in) {
		t.Fatal("refused apply must not write")
	}
	gitRun(t, dir, "add", "oracle.jsonl")
	// tracked but dirty (staged, uncommitted) → refuse
	if _, err := requarantineFile(p, "v0.40.4", true); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty oracle must refuse -apply: %v", err)
	}
	gitRun(t, dir, "commit", "-q", "-m", "oracle")
	// outside a repo entirely → refuse
	loose := filepath.Join(t.TempDir(), "oracle.jsonl")
	if err := os.WriteFile(loose, in, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := requarantineFile(loose, "v0.40.4", true); err == nil || !strings.Contains(err.Error(), "not tracked") {
		t.Fatalf("oracle outside git must refuse -apply: %v", err)
	}

	rep, err := requarantineFile(p, "v0.40.4", true)
	if err != nil || len(rep.Matched) != 5 {
		t.Fatalf("apply: %v, matched %d", err, len(rep.Matched))
	}
	out, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\r\n")
	if len(lines) != 12 {
		t.Fatalf("line count changed: %d\n%s", len(lines), out)
	}
	if strings.Count(string(out), `"quarantined":"v0.40.4"`) != 6 { // 5 stamped + the pre-existing one
		t.Fatalf("stamp count wrong:\n%s", out)
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
	rep2, err := requarantineFile(p, "v0.40.4", true)
	if err != nil || len(rep2.Matched) != 0 {
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
	if code != 0 || !strings.Contains(so, "DRY RUN") || !strings.Contains(so, "5 evidence row(s)") {
		t.Fatalf("dry run: exit %d\n%s\n%s", code, so, se)
	}
	if b, _ := os.ReadFile(p); string(b) != string(in) {
		t.Fatal("dry run wrote the oracle")
	}
	code, so, se = intRun(t, dir, "-requarantine", p, "-apply")
	if code != 2 || !strings.Contains(se, "commit the oracle first") {
		t.Fatalf("apply on an untracked oracle must refuse: exit %d\n%s\n%s", code, so, se)
	}
	if b, _ := os.ReadFile(p); string(b) != string(in) {
		t.Fatal("refused apply wrote the oracle")
	}
}
