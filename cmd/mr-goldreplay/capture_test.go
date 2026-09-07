package main

// The candidate-diff capture seam (v0.40.4, A1+A3). On 2026-09-06 the
// codex/claude/copilot agentic-coding cells were failing at goldverify's
// `git apply` because the capture (a) took git's COMBINED output, so the CRLF
// warnings git prints on stderr became patch bytes, and (b) swept the agent's
// build caches into the patch as binary stanzas the verifier cannot apply.
// Both were recorded as measured model failures. These tests pin the split,
// the deny-list, the provenance, the resume key, the anti-laundering rule
// and the worktree hygiene — each with the mutation that would silence it.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/meta-router/internal/goldtask"
	"github.com/dmmdea/meta-router/internal/policyeval"
)

// isolateGit pins the global and system git config to empty files so the
// operator's own core.autocrlf / safecrlf cannot make a test pass or fail
// by accident; each test sets the repo-local values it needs.
func isolateGit(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty-gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_SYSTEM", empty)
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newTempRepo is a one-commit repo (a.txt, LF endings) and its HEAD sha.
func newTempRepo(t *testing.T) (dir, head string) {
	t.Helper()
	isolateGit(t)
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@example.invalid")
	gitRun(t, dir, "config", "user.name", "goldreplay-test")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	gitRun(t, dir, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir, strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
}

func mustWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// (a)+(b) The capture must carry no git stderr, no build cache, no binary
// stanza — and the resulting patch must APPLY at the parent. (b) is the
// mutation guard: the combined-output capture of the same worktree MUST
// contain the warning, or this test could not fail if the split regressed.
func TestWorktreeDiffExcludesGitStderr(t *testing.T) {
	dir, head := newTempRepo(t)
	// The live shape: autocrlf on, safecrlf=warn (measured: safecrlf=false
	// suppresses the warning, so without pinning it the test greens on a
	// machine where the bug is live).
	gitRun(t, dir, "config", "core.autocrlf", "true")
	gitRun(t, dir, "config", "core.safecrlf", "warn")
	mustWrite(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"))                       // modified tracked, LF
	mustWrite(t, filepath.Join(dir, "b.txt"), []byte("new\nfile\n"))                             // untracked, LF
	mustWrite(t, filepath.Join(dir, ".go-cache", "00", "0074-d"), []byte("\x00\x01\x02bin\x00")) // binary cache object
	mustWrite(t, filepath.Join(dir, "sub", ".go-cache", "01", "x-d"), []byte("\x00\xff"))        // nested cache
	mustWrite(t, filepath.Join(dir, "node_modules", "m", "index.js"), []byte("x\n"))

	// (b) mutation guard: the OLD capture shape reproduces the defect here.
	gitRun(t, dir, "add", "-N", ".")
	combined, _ := exec.Command("git", "-C", dir, "diff", "HEAD").CombinedOutput()
	if !strings.Contains(string(combined), "warning: in the working copy of") {
		t.Fatalf("fixture does not reproduce the bug: combined output carries no CRLF warning\n%s", combined)
	}
	if !strings.Contains(string(combined), ".go-cache") || !strings.Contains(string(combined), "Binary files") {
		t.Fatalf("fixture does not reproduce the bug: combined output carries no cache/binary stanza\n%s", combined)
	}
	gitRun(t, dir, "reset", "-q") // clear the blanket intent-to-add before the real capture

	// (a) the real capture.
	diff, err := captureWorktreeDiff(dir, 60)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	for _, forbidden := range []string{"warning: in the working copy of", ".go-cache", "Binary files ", "node_modules"} {
		if strings.Contains(diff, forbidden) {
			t.Fatalf("capture must not contain %q:\n%s", forbidden, diff)
		}
	}
	if !strings.Contains(diff, "+++ b/a.txt") || !strings.Contains(diff, "+++ b/b.txt") || !strings.Contains(diff, "+three") {
		t.Fatalf("capture lost the agent's real edits:\n%s", diff)
	}
	if bad := patchGrammarViolation(diff); bad != "" {
		t.Fatalf("clean capture flagged by the grammar gate: %q", bad)
	}
	// The assertion that matters: it applies at the parent. A stdout-only
	// capture of the real AC-03 worktree still failed on a cache object
	// (`cannot apply binary patch … without full index line`).
	pf := filepath.Join(t.TempDir(), "cand.diff")
	mustWrite(t, pf, []byte(diff))
	fresh := filepath.Join(t.TempDir(), "fresh")
	gitRun(t, dir, "worktree", "add", "--detach", "-q", fresh, head)
	defer func() { _, _, _ = gitOut(dir, 60, "worktree", "remove", "--force", fresh) }()
	if out, err := exec.Command("git", "-C", fresh, "apply", "--whitespace=nowarn", "--recount", pf).CombinedOutput(); err != nil {
		t.Fatalf("captured patch must apply at its parent: %v\n%s\n--- patch ---\n%s", err, out, diff)
	}
}

// (c) The filter is a DENY-list: an untracked non-source fixture survives.
// AC-07 requires creating `internal/catalog/testdata/*.md`; an extension
// allowlist would make that cell unpassable forever.
func TestCaptureKeepsNewNonSourceFiles(t *testing.T) {
	dir, _ := newTempRepo(t)
	mustWrite(t, filepath.Join(dir, "internal", "catalog", "testdata", "x.md"), []byte("# fixture\n"))
	mustWrite(t, filepath.Join(dir, ".gocache", "obj"), []byte("\x00"))
	diff, err := captureWorktreeDiff(dir, 60)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(diff, "+++ b/internal/catalog/testdata/x.md") || !strings.Contains(diff, "+# fixture") {
		t.Fatalf("untracked .md fixture must survive into the patch:\n%s", diff)
	}
	if strings.Contains(diff, ".gocache") {
		t.Fatalf("cache dir leaked into the patch:\n%s", diff)
	}
	// Empty worktree → empty diff, nil error: the fail-open shape the
	// printed fallback relies on.
	clean, _ := newTempRepo(t)
	if d, err := captureWorktreeDiff(clean, 60); d != "" || err != nil {
		t.Fatalf("clean worktree must read as no diff, nil error: %q %v", d, err)
	}
}

// A failing capture is a HOLE, never a scored row: the old code discarded
// the add -N error and fell through to "no diff in output", which is scored.
func TestCaptureFailureIsAnError(t *testing.T) {
	isolateGit(t)
	notRepo := t.TempDir()
	if _, err := captureWorktreeDiff(notRepo, 60); err == nil || !strings.Contains(err.Error(), "git add -N") {
		t.Fatalf("a non-repo cwd must fail loudly at add -N, got %v", err)
	}
}

// The pre-apply gate scans the WHOLE stream: leading junk applies at exit 0
// and mid-hunk junk is the fatal one.
func TestPatchGrammarViolation(t *testing.T) {
	clean := "diff --git a/x.go b/x.go\nindex 1234567..89abcde 100644\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,3 @@\n a\n+b\n-c\n\\ No newline at end of file\ndiff --git a/n.md b/n.md\nnew file mode 100644\n--- /dev/null\n+++ b/n.md\n@@ -0,0 +1 @@\n+# n\n"
	if bad := patchGrammarViolation(clean); bad != "" {
		t.Fatalf("clean diff flagged: %q", bad)
	}
	if bad := patchGrammarViolation(strings.ReplaceAll(clean, "\n", "\r\n")); bad != "" {
		t.Fatalf("CRLF diff flagged: %q", bad)
	}
	spliced := strings.Replace(clean, "+b\n", "+b\nwarning: in the working copy of 'x.go', LF will be replaced by CRLF the next time Git touches it\n", 1)
	if bad := patchGrammarViolation(spliced); !strings.HasPrefix(bad, "warning: in the working copy") {
		t.Fatalf("mid-hunk stderr splice must be caught, got %q", bad)
	}
	binary := clean + "diff --git a/.go-cache/x-d b/.go-cache/x-d\nnew file mode 100644\nindex 0000000..1111111\nBinary files /dev/null and b/.go-cache/x-d differ\n"
	if bad := patchGrammarViolation(binary); !strings.HasPrefix(bad, "Binary files ") {
		t.Fatalf("binary stanza must be caught, got %q", bad)
	}
	if bad := patchGrammarViolation("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\nrename from y\nrename to z\nsimilarity index 90%\n"); bad != "" {
		t.Fatalf("extended headers are grammar: %q", bad)
	}
}

// truncateDiff now receipts what it cut; a clean diff cuts nothing.
func TestTruncateDiffReceiptsCutLines(t *testing.T) {
	clean := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"
	if d, cut := truncateDiffN(clean); d != strings.TrimSuffix(clean, "\n") || cut != 0 {
		t.Fatalf("clean: cut=%d %q", cut, d)
	}
	if _, cut := truncateDiffN(clean + "\nThat should fix it.\nLet me know.\n"); cut != 2 {
		t.Fatalf("prose tail must be counted as SUBSTANTIVE lines (not the blank separator or the trailing split artifact), got %d", cut)
	}
	if _, cut := truncateDiffN(clean + "\nThat should fix it.\nLet me know."); cut != 2 {
		t.Fatalf("the receipt must not depend on whether the stream ended with a newline, got %d", cut)
	}
}

// (e) Anti-laundering: a WORKTREE-sourced diff that fails at `git apply` is
// a harness fault (hole); a PRINTED diff that fails to apply is the model's
// failure (evidence). A worktree diff failing at the TEST stage stays
// evidence — only the apply stage is structurally ours.
func TestHarnessCorruptedApplyIsHole(t *testing.T) {
	applyFail := []byte(`{"task":"AC-02","pass":false,"detail":"git apply: exit status 128\nerror: corrupt patch at line 70"}`)
	r := Row{Dispatched: true, OutcomeClass: "ok"}
	applyVerifyOutcome(&r, applyFail, realExitError(t, 1), diffSourceWorktree)
	if r.OutcomeClass != "verify_error" || r.VerifierPass || policyeval.IsEvidence(r.Dispatched, r.OutcomeClass) {
		t.Fatalf("worktree apply failure must be a hole: %+v", r)
	}
	if !strings.Contains(r.Note, "harness fault") || !strings.Contains(r.Note, "corrupt patch") {
		t.Fatalf("hole must say why and carry the verdict: %q", r.Note)
	}
	for _, src := range []string{diffSourcePrinted, diffSourceRaw, ""} {
		r = Row{Dispatched: true, OutcomeClass: "ok"}
		applyVerifyOutcome(&r, applyFail, realExitError(t, 1), src)
		if r.OutcomeClass != "ok" || r.VerifierPass || !policyeval.IsEvidence(r.Dispatched, r.OutcomeClass) {
			t.Fatalf("%q apply failure must stay a measured failure: %+v", src, r)
		}
		if !strings.HasPrefix(r.Note, "verify-fail: git apply") {
			t.Fatalf("%q note: %q", src, r.Note)
		}
	}
	testFail := []byte(`{"task":"AC-02","pass":false,"detail":"go test: exit status 1\n--- FAIL: TestX"}`)
	r = Row{Dispatched: true, OutcomeClass: "ok"}
	applyVerifyOutcome(&r, testFail, realExitError(t, 1), diffSourceWorktree)
	if r.OutcomeClass != "ok" || r.VerifierPass || !policyeval.IsEvidence(r.Dispatched, r.OutcomeClass) {
		t.Fatalf("worktree diff failing held-out tests is evidence: %+v", r)
	}
}

// (f) Provenance through the real replay path, three shapes: the agent
// edited in place (worktree), printed a diff (printed-decoded), or wrote
// prose only (none). Plus the end-to-end anti-laundering rule and the A3
// hygiene: nothing left under the worktree root afterwards.
func TestReplayOneRecordsDiffProvenance(t *testing.T) {
	dir, head := newTempRepo(t)
	root := t.TempDir()
	prev := worktreeRoot
	worktreeRoot = func() string { return root }
	defer func() { worktreeRoot = prev }()
	task := goldtask.Task{ID: "AC-T1", Class: "agentic-coding", Split: "tuning", Repo: "tr", Prompt: "edit",
		Verify: goldtask.VerifySpec{Kind: "vgo", Repo: "tr", Parent: head, Resolving: head, Pkgs: []string{"./..."}}}
	cfg := policyeval.Config{Lane: "codex", Model: "gpt-5.6-terra", Effort: "high"}
	repos := "tr=" + dir
	leftovers := func() []string {
		m, _ := filepath.Glob(filepath.Join(root, "goldreplay-*"))
		return m
	}

	// worktree: the fake writes b.txt into its -cwd; the fake verifier passes.
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", "edited in place\n")
	t.Setenv("GOLDREPLAY_FAKE_ORCH_EXIT", "0")
	t.Setenv("GOLDREPLAY_FAKE_ORCH_WRITE", "b.txt=hello\n")
	t.Setenv("GOLDREPLAY_FAKE_VERIFY_STDOUT", `{"pass":true}`)
	t.Setenv("GOLDREPLAY_FAKE_VERIFY_EXIT", "0")
	t.Setenv("GOLDREPLAY_FAKE_VERIFY_EXPECT", "+hello") // the verifier must receive the agent's edit
	row := replayOne(task, cfg, 1, os.Args[0], os.Args[0], repos, 30, 10, "")
	if row.OutcomeClass != "ok" || !row.VerifierPass || row.DiffSource != diffSourceWorktree || row.DiffTruncatedLines != 0 {
		t.Fatalf("worktree case: %+v", row)
	}
	// Precedence: the agent edits in place AND prints a diff — the worktree
	// wins, the printed text is ignored, nothing is "truncated".
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", `{"type":"assistant.message","data":{"content":"diff --git a/zz b/zz\n--- a/zz\n+++ b/zz\n@@ -1 +1 @@\n-q\n+r\n\nAlso narrating."}}`+"\n")
	row = replayOne(task, cfg, 1, os.Args[0], os.Args[0], repos, 30, 10, "")
	if row.OutcomeClass != "ok" || !row.VerifierPass || row.DiffSource != diffSourceWorktree || row.DiffTruncatedLines != 0 {
		t.Fatalf("worktree must take precedence over a printed diff: %+v", row)
	}
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", "edited in place\n")
	t.Setenv("GOLDREPLAY_FAKE_VERIFY_EXPECT", "")
	if l := leftovers(); len(l) != 0 {
		t.Fatalf("A3: worktree/diff left behind: %v", l)
	}

	// worktree + apply-stage failure → hole (the whole point of A1).
	t.Setenv("GOLDREPLAY_FAKE_VERIFY_STDOUT", `{"pass":false,"detail":"git apply: exit status 128\nerror: corrupt patch"}`)
	t.Setenv("GOLDREPLAY_FAKE_VERIFY_EXIT", "1")
	row = replayOne(task, cfg, 1, os.Args[0], os.Args[0], repos, 30, 10, "")
	if row.OutcomeClass != "verify_error" || row.DiffSource != diffSourceWorktree || policyeval.IsEvidence(row.Dispatched, row.OutcomeClass) {
		t.Fatalf("worktree apply failure must be a hole end to end: %+v", row)
	}

	// printed-decoded: nothing edited; a copilot-shaped event carries the
	// diff plus trailing prose (2 substantive lines cut). The same apply
	// failure stays a measured failure.
	t.Setenv("GOLDREPLAY_FAKE_ORCH_WRITE", "")
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", `{"type":"assistant.message","data":{"content":"Fix:\n\ndiff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+uno\n\nThat should do it.\nDone.","model":"gpt-5.6-terra"}}`+"\n")
	row = replayOne(task, cfg, 1, os.Args[0], os.Args[0], repos, 30, 10, "")
	if row.OutcomeClass != "ok" || row.VerifierPass || row.DiffSource != diffSourcePrinted || row.DiffTruncatedLines != 2 {
		t.Fatalf("printed case: %+v", row)
	}
	if !policyeval.IsEvidence(row.Dispatched, row.OutcomeClass) {
		t.Fatalf("a printed patch that does not apply is the model's failure: %+v", row)
	}

	// none: prose only.
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", `{"type":"assistant.message","data":{"content":"I could not make the change."}}`+"\n")
	row = replayOne(task, cfg, 1, os.Args[0], os.Args[0], repos, 30, 10, "")
	if row.OutcomeClass != "ok" || row.VerifierPass || row.DiffSource != diffSourceNone || row.Note != "no diff in output" {
		t.Fatalf("prose-only case: %+v", row)
	}
	if l := leftovers(); len(l) != 0 {
		t.Fatalf("A3: leftovers after the run: %v", l)
	}
}

// A3: the note carries the FULL worktree-add stderr (the fatal line comes
// second; firstLine kept only the "Preparing worktree" banner), and two
// cells of the same identity never share a path.
func TestWorktreeAddFailureNoteAndNonce(t *testing.T) {
	dir, head := newTempRepo(t)
	pre := filepath.Join(t.TempDir(), "goldreplay-ac-01-codex-1-pre")
	mustWrite(t, filepath.Join(pre, "stale.txt"), []byte("x"))
	_, stderr, err := gitOut(dir, 30, "worktree", "add", "--detach", pre, head)
	if err == nil {
		t.Fatal("worktree add into a non-empty path must fail")
	}
	note := boundedText(stderr, err, 400)
	if !strings.Contains(note, "already exists") {
		t.Fatalf("note must carry the fatal line: %q", note)
	}
	// Mutation guard: the old first-line note hid it behind the banner.
	if old := firstLine(stderr, err); strings.Contains(old, "already exists") {
		t.Fatalf("fixture does not reproduce the two-line fatal (old note %q)", old)
	}
	p1 := worktreePath("AC-01", "codex", 1, cellNonce())
	p2 := worktreePath("AC-01", "codex", 1, cellNonce())
	if p1 == p2 || candidateDiffPath("AC-01", "codex", 1, cellNonce()) == candidateDiffPath("AC-01", "codex", 1, cellNonce()) {
		t.Fatalf("same identity must not share a temp path: %s", p1)
	}
	if !strings.HasPrefix(filepath.Base(p1), "goldreplay-ac-01-codex-1-") {
		t.Fatalf("path shape: %s", p1)
	}
}

// (d) diff_source is provenance, not identity: rows carrying it are still
// "already recorded" and a rerun leaves the oracle byte-identical.
// Mutation proof: adding DiffSource to rowKey's format string makes this red.
func TestIntegrationDiffSourceDoesNotRekeyResume(t *testing.T) {
	rows := `{"task":"T-01","class":"research","lane":"local","model":"gemma4-cascade","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"diff_source":"worktree"}
{"task":"T-02","class":"research","lane":"local","model":"gemma4-cascade","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"diff_source":"printed-decoded","diff_truncated_lines":3}
`
	dir, goldset, oracle := intFixture(t, rows)
	code, so, se := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-lanes", "local",
		"-local-model", "gemma4-cascade", "-local-effort", "high",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, so, se)
	}
	if !strings.Contains(so, "(0 run now, 2 already recorded)") {
		t.Fatalf("rows with provenance must resume as recorded: %s", so)
	}
	b, _ := os.ReadFile(oracle)
	if string(b) != rows {
		t.Fatalf("oracle must be byte-identical after a fully-recorded run:\n%s", b)
	}
}

// The timeout is honoured against a git that BLOCKS — on the operator's
// PATH git is the Git for Windows launcher whose real child holds the pipes,
// so without WaitDelay (and the mingw64 substitution) Run never returns.
// A listener that accepts and never answers makes `git ls-remote` block
// natively, no shell children involved.
func TestGitOutTimeoutReturnsOnBlockingGit(t *testing.T) {
	isolateGit(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, never reply
		}
	}()
	prev := gitWaitDelay
	gitWaitDelay = 2 * time.Second
	defer func() { gitWaitDelay = prev }()
	start := time.Now()
	_, _, err = gitOut(t.TempDir(), 1, "ls-remote", "git://"+ln.Addr().String()+"/x")
	el := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timeout after 1s") {
		t.Fatalf("blocking git must report the timeout, got %v after %s", err, el)
	}
	if el > 8*time.Second {
		t.Fatalf("gitOut returned only after %s — the kill did not reach the process that holds the pipes", el)
	}
}

// WaitDelay is the guard for a CHILD of git that outlives the kill while
// holding git's inherited stderr — a hook, or the launcher's real git. A
// git `ext::` remote helper that sleeps is that child: killing git leaves
// it holding the pipe, and without WaitDelay Wait never returns. Bounded
// by a goroutine so the mutant reads as a failure, not a hung suite.
func TestGitOutWaitDelayReleasesHeldPipe(t *testing.T) {
	isolateGit(t)
	prev := gitWaitDelay
	gitWaitDelay = 2 * time.Second
	defer func() { gitWaitDelay = prev }()
	exe := filepath.ToSlash(os.Args[0])
	t.Setenv("GOLDREPLAY_FAKE_HOLD", "1")
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		// The system temp dir, not t.TempDir(): the sleeping helper inherits
		// the cwd and outlives the test, which would fail TempDir's cleanup.
		_, _, err := gitOut(os.TempDir(), 1, "-c", "protocol.ext.allow=always", "ls-remote", "ext::"+exe)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timeout after 1s") {
			t.Fatalf("held pipe must still end in the timeout error, got %v after %s", err, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gitOut did not return within 5s (timeout 1s + WaitDelay 2s): a child holding git's stderr blocked Wait (WaitDelay missing)")
	}
}

// The capture-failure hole at the replayOne seam: the agent wrecks its
// worktree, git fails inside the capture, the row is verify_error with the
// error named — never "ok / no diff in output".
func TestReplayOneCaptureFailureIsHole(t *testing.T) {
	dir, head := newTempRepo(t)
	root := t.TempDir()
	prev := worktreeRoot
	worktreeRoot = func() string { return root }
	defer func() { worktreeRoot = prev }()
	task := goldtask.Task{ID: "AC-T2", Class: "agentic-coding", Split: "tuning", Repo: "tr", Prompt: "edit",
		Verify: goldtask.VerifySpec{Kind: "vgo", Repo: "tr", Parent: head, Resolving: head, Pkgs: []string{"./..."}}}
	cfg := policyeval.Config{Lane: "codex", Model: "gpt-5.6-terra", Effort: "high"}
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", "edited in place\n")
	t.Setenv("GOLDREPLAY_FAKE_ORCH_EXIT", "0")
	t.Setenv("GOLDREPLAY_FAKE_ORCH_BREAK", "1")
	row := replayOne(task, cfg, 1, os.Args[0], os.Args[0], "tr="+dir, 30, 10, "")
	if row.OutcomeClass != "verify_error" || policyeval.IsEvidence(row.Dispatched, row.OutcomeClass) {
		t.Fatalf("a failed capture must be a hole: %+v", row)
	}
	if !strings.HasPrefix(row.Note, "worktree capture: git add -N: ") || !strings.Contains(row.Note, "stderr:") {
		t.Fatalf("hole note must name the failing git step and carry stderr: %q", row.Note)
	}
	t.Setenv("GOLDREPLAY_FAKE_ORCH_BREAK", "")

	// The grammar gate at the seam: a binary the deny-list does not cover
	// (a PNG fixture) makes the capture carry a "Binary files" stanza the
	// verifier cannot apply — a hole naming the stanza, not a model failure.
	t.Setenv("GOLDREPLAY_FAKE_ORCH_WRITE", "assets/logo.png=hex:89504e470d0a1a0a0000000d49484452")
	row = replayOne(task, cfg, 1, os.Args[0], os.Args[0], "tr="+dir, 30, 10, "")
	if row.OutcomeClass != "verify_error" || !strings.Contains(row.Note, "not a patch: Binary files") {
		t.Fatalf("binary stanza must be a hole at the seam: %+v", row)
	}
	t.Setenv("GOLDREPLAY_FAKE_ORCH_WRITE", "")
	if m, _ := filepath.Glob(filepath.Join(root, "goldreplay-*")); len(m) != 0 {
		t.Fatalf("leftovers after hole rows: %v", m)
	}
}

// The worktree-add note through replayOne itself: pin the nonce, pre-create
// the path, and the note must carry the fatal line (the whole stderr — the
// banner comes first, and a first-line note kept ONLY the banner).
func TestReplayOneWorktreeAddNoteCarriesFatalLine(t *testing.T) {
	dir, head := newTempRepo(t)
	root := t.TempDir()
	prevRoot, prevNonce := worktreeRoot, cellNonce
	worktreeRoot = func() string { return root }
	cellNonce = func() string { return "pinned" }
	defer func() { worktreeRoot, cellNonce = prevRoot, prevNonce }()
	mustWrite(t, filepath.Join(worktreePath("AC-T3", "codex", 1, "pinned"), "stale.txt"), []byte("x"))
	task := goldtask.Task{ID: "AC-T3", Class: "agentic-coding", Split: "tuning", Repo: "tr", Prompt: "edit",
		Verify: goldtask.VerifySpec{Kind: "vgo", Repo: "tr", Parent: head, Resolving: head, Pkgs: []string{"./..."}}}
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", "never reached\n")
	row := replayOne(task, policyeval.Config{Lane: "codex", Model: "m", Effort: "high"}, 1, os.Args[0], os.Args[0], "tr="+dir, 30, 10, "")
	if row.OutcomeClass != "error" || !strings.Contains(row.Note, "already exists") {
		t.Fatalf("note must carry the fatal line: %+v", row)
	}
}

// removeWorktree's fallback: a tree git does not know about (no admin entry)
// is still reclaimed by os.RemoveAll.
func TestRemoveWorktreeFallsBackToRemoveAll(t *testing.T) {
	dir, _ := newTempRepo(t)
	stray := filepath.Join(t.TempDir(), "goldreplay-stray")
	mustWrite(t, filepath.Join(stray, "f.txt"), []byte("x"))
	removeWorktree(dir, stray)
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("fallback must reclaim a tree git cannot remove")
	}
}

// boundedText: a cut is receipted and never lands mid-rune.
func TestBoundedTextReceiptsTheCut(t *testing.T) {
	s := boundedText([]byte("fatal: 'C:/tmp/ñandú/goldreplay-x' already exists\nsecond line"), nil, 20)
	if !strings.Contains(s, "…(+") || !utf8.ValidString(s) {
		t.Fatalf("cut must be marked and rune-safe: %q", s)
	}
	if got := boundedText([]byte("a\r\nb"), nil, 100); got != "a | b" {
		t.Fatalf("lines fold: %q", got)
	}
}
