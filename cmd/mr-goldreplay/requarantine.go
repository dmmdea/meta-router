package main

// -requarantine: quarantine the oracle rows the v0.40.4 capture defect
// poisoned. Before that fix mr-goldreplay spliced git's stderr into the
// candidate patch and swept the agent's build caches in as binary stanzas;
// the verifier's `git apply` then failed and the cell was recorded as a
// MEASURED model failure (`ok` / `verifier_pass:false`, and
// `dispatched-not-ok` on copilot). Those rows are holes that were scored.
//
// The mode is ADDITIVE and DRY-RUN BY DEFAULT. It never deletes a row and
// never rewrites an outcome class: it stamps `quarantined` (the instrument
// version that made the row a hole) and `quarantine_reason` onto each
// matched line by byte splice, and policyeval.IsEvidence — the ONE
// evidence definition — treats a quarantined row as a hole. Resume then
// refills the cell; the row's model still counts for the drift guard's
// identity index, because a refill at the same pin is a re-measurement,
// not a re-key. `-apply` is the operator's keystroke: applying this
// withdraws Astra's 0.18 agentic-coding / 0.65 task-mean pending re-run,
// moves claude/copilot/glm/luna figures too, and creates a paid
// re-dispatch obligation on the copilot lane.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmmdea/meta-router/internal/goldtask"
)

// quarantineSignatures is the UNION of the verifier notes the harness
// defect produced, measured across lanes. The three-signature version
// (`warning: in the working copy`, `lacks filename information`, `No valid
// patches in input`) was lane-biased: it caught 7 codex + 4 copilot rows and
// ZERO claude / glm rows, missing the `recount: unexpected line` class that
// carries the cleanest cross-vendor proof (claude AC-02 and codex/luna AC-02
// with the identical split of cmd/mr-index/main.go:455).
var quarantineSignatures = []string{
	"warning: in the working copy",
	"lacks filename information",
	"recount: unexpected line",
	"corrupt patch at",
	"No valid patches in input",
}

// quarantineNotePrefix is the note shape the defect produced: goldverify
// failed at the `git apply` STAGE (applyVerifyOutcome writes
// "verify-fail: " + verdictDetail, and verdictDetail is "<stage>: <err> |
// <tail>"). Without this gate the signature scan reads the WHOLE note,
// including the tail of a `go test` failure — and the goldset is
// self-hosting (its tasks edit THIS repo, whose sources and fixtures
// contain the signature strings verbatim), so a held-out test whose output
// prints "corrupt patch at" would be swept in as a harness fault and a
// genuine model failure would be laundered into a hole.
var quarantineNotePrefix = "verify-fail: " + goldtask.StageGitApply + ":"

const quarantineReason = "harness-corrupted candidate diff (git stderr spliced into the patch / build cache swept in) recorded as a model failure; instrument fixed in v0.40.4; hole, re-measure"

// quarantineMatch is one row the selector picked, with its 0-based line.
type quarantineMatch struct {
	Line int
	Row  Row
}

// laneStat is an evidence pass-rate cell.
type laneStat struct{ N, Pass int }

func (s laneStat) rate() string {
	if s.N == 0 {
		return "n/a (n=0)"
	}
	return fmt.Sprintf("%.2f (n=%d)", float64(s.Pass)/float64(s.N), s.N)
}

// quarantineReport is what the dry run prints and what -apply acts on.
type quarantineReport struct {
	Matched       []quarantineMatch
	ByLaneModel   map[string]map[string]int // "lane/model" → outcome_class → count
	Before        map[string]laneStat       // lane → all-class evidence pass rate
	After         map[string]laneStat
	BeforeAgentic map[string]laneStat // lane → agentic-coding evidence pass rate
	AfterAgentic  map[string]laneStat
	// The boundary, printed so the operator sees what the selector REFUSED
	// rather than inferring it from a count that got smaller.
	ExcludedStage   int // signature present, but not an apply-stage failure
	ExcludedPostFix int // signature present at the apply stage, but the row carries diff provenance ⇒ written under v0.40.4+, so its failure is the model's
	Undecodable     int // non-empty lines the selector could not read as a row
}

// quarantineSelect matches every EVIDENCE-bearing row (all classes,
// including dispatched-not-ok — four of the copilot rows are that class and
// it is evidence) whose note carries any signature and that is not already
// quarantined. Holes (deferred, verify_error, error, undispatched) are left
// alone: they are not scored today, and stamping them would misdescribe
// their cause.
func quarantineSelect(in []byte) quarantineReport {
	rep := quarantineReport{
		ByLaneModel:   map[string]map[string]int{},
		Before:        map[string]laneStat{},
		After:         map[string]laneStat{},
		BeforeAgentic: map[string]laneStat{},
		AfterAgentic:  map[string]laneStat{},
	}
	for i, line := range bytes.Split(in, []byte("\n")) {
		body := bytes.TrimSpace(line)
		if len(body) == 0 {
			continue
		}
		// Every non-empty line the selector cannot read as an identified row
		// is COUNTED, not silently dropped: a torn line, a non-JSON line and
		// a row without lane/task are all invisible to the selector, and a
		// report that does not say so reads as full coverage.
		var r Row
		if body[0] != '{' || json.Unmarshal(body, &r) != nil || r.Task == "" || r.Lane == "" {
			rep.Undecodable++
			continue
		}
		if !rowIsEvidence(r) {
			continue
		}
		hit := false
		for _, sig := range quarantineSignatures {
			if strings.Contains(r.Note, sig) {
				hit = true
				break
			}
		}
		// Two bounds on the signature, each closing a laundering path.
		// STAGE: the failure must be `git apply` itself, not a signature
		// string appearing in some other stage's captured output.
		// PROVENANCE: the row must predate the fix. A row carrying
		// diff_source was written by v0.40.4+, where a worktree diff that
		// fails to apply is ALREADY a hole and a printed diff that fails to
		// apply is by design the model's failure (the anti-laundering rule);
		// quarantining those would relabel measured failures as harness
		// faults. Absent means unknown, which is exactly the pre-fix
		// population this mode exists for (all 22 live rows are absent).
		if hit {
			switch {
			case !strings.HasPrefix(r.Note, quarantineNotePrefix):
				rep.ExcludedStage++
				hit = false
			case r.DiffSource != "":
				rep.ExcludedPostFix++
				hit = false
			}
		}
		add := func(m map[string]laneStat, k string) {
			s := m[k]
			s.N++
			if r.VerifierPass {
				s.Pass++
			}
			m[k] = s
		}
		add(rep.Before, r.Lane)
		if r.Class == "agentic-coding" {
			add(rep.BeforeAgentic, r.Lane)
		}
		if !hit {
			add(rep.After, r.Lane)
			if r.Class == "agentic-coding" {
				add(rep.AfterAgentic, r.Lane)
			}
			continue
		}
		rep.Matched = append(rep.Matched, quarantineMatch{Line: i, Row: r})
		lm := r.Lane + "/" + displayModel(strings.TrimSpace(r.Model))
		if rep.ByLaneModel[lm] == nil {
			rep.ByLaneModel[lm] = map[string]int{}
		}
		rep.ByLaneModel[lm][r.OutcomeClass]++
		// A lane that loses rows but keeps none still needs an After entry
		// so the table shows the drop, not a missing line.
		if _, ok := rep.After[r.Lane]; !ok {
			rep.After[r.Lane] = laneStat{}
		}
		if r.Class == "agentic-coding" {
			if _, ok := rep.AfterAgentic[r.Lane]; !ok {
				rep.AfterAgentic[r.Lane] = laneStat{}
			}
		}
	}
	return rep
}

// quarantineMode is what the report header must tell the operator: a dry
// run wrote nothing, an APPLY wrote, and an ABORTED apply wrote NOTHING —
// the third case used to print the same "APPLY" header as a successful one,
// so a failed apply read like a completed one with an error tacked on.
type quarantineMode int

const (
	qDryRun quarantineMode = iota
	qApplied
	qAborted
)

// render prints the plan the way the operator reads it before -apply.
func (rep quarantineReport) render(stamp string, mode quarantineMode) string {
	var b strings.Builder
	modeText := map[quarantineMode]string{
		qDryRun:  "DRY RUN — nothing written; pass -apply to stamp",
		qApplied: "APPLY",
		qAborted: "APPLY ABORTED — nothing was written; the oracle is unchanged",
	}[mode]
	fmt.Fprintf(&b, "requarantine (%s): %d evidence row(s) match the harness-corruption signatures; stamp quarantined=%q\n", modeText, len(rep.Matched), stamp)
	keys := make([]string, 0, len(rep.ByLaneModel))
	for k := range rep.ByLaneModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cls := rep.ByLaneModel[k]
		ck := make([]string, 0, len(cls))
		for c := range cls {
			ck = append(ck, c)
		}
		sort.Strings(ck)
		parts := make([]string, 0, len(ck))
		n := 0
		for _, c := range ck {
			parts = append(parts, fmt.Sprintf("%s:%d", c, cls[c]))
			n += cls[c]
		}
		fmt.Fprintf(&b, "  %-40s %3d  %s\n", k, n, strings.Join(parts, " "))
	}
	lanes := make([]string, 0, len(rep.Before))
	for l := range rep.Before {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)
	b.WriteString("  per-lane evidence pass rate, before → after (all classes | agentic-coding):\n")
	for _, l := range lanes {
		if rep.Before[l] == rep.After[l] {
			continue // untouched lane: no line, no noise
		}
		fmt.Fprintf(&b, "  %-9s %s → %s | %s → %s\n", l,
			rep.Before[l].rate(), rep.After[l].rate(), rep.BeforeAgentic[l].rate(), rep.AfterAgentic[l].rate())
	}
	if rep.ExcludedStage > 0 || rep.ExcludedPostFix > 0 || rep.Undecodable > 0 {
		fmt.Fprintf(&b, "  boundary: %d signature-carrying row(s) NOT selected — %d failed at another verifier stage, %d carry diff provenance (written under v0.40.4+, so an apply failure there is the model's); %d line(s) undecodable\n",
			rep.ExcludedStage+rep.ExcludedPostFix, rep.ExcludedStage, rep.ExcludedPostFix, rep.Undecodable)
	}
	if len(rep.Matched) > 0 && mode != qAborted {
		b.WriteString("  receipt: these rows become HOLES (never evidence, refilled by the next sweep at the same pin — no drift refusal). Applying withdraws every figure derived from them until re-measured, and every refill is a re-dispatch: a paid one on copilot (month credits) and a weekly-window one on codex.\n")
	}
	return b.String()
}

// spliceMembers appends JSON members to the object on one oracle line by
// byte splice (the migrate-effort discipline: never decode-and-re-marshal —
// unknown fields, key order and integer widths survive; CR terminators
// round-trip). members is `"k":v,...` without the leading comma.
func spliceMembers(line []byte, members string) ([]byte, error) {
	cr := []byte(nil)
	body := line
	if n := len(body); n > 0 && body[n-1] == '\r' {
		cr, body = body[n-1:], body[:n-1]
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("line is not valid JSON")
	}
	close := bytes.LastIndexByte(body, '}')
	if close < 0 {
		return nil, fmt.Errorf("no closing brace")
	}
	member := []byte("," + members)
	if open := bytes.IndexByte(body, '{'); open >= 0 && len(bytes.TrimSpace(body[open+1:close])) == 0 {
		member = member[1:]
	}
	out := append(append(append([]byte{}, body[:close]...), member...), body[close:]...)
	return append(out, cr...), nil
}

// requarantine stamps the matched lines and returns the rewritten bytes.
// FIXED POINT: stamped rows no longer match (they are no longer evidence),
// so a second pass changes nothing.
func requarantine(in []byte, rep quarantineReport, stamp string) ([]byte, error) {
	want := map[int]bool{}
	for _, m := range rep.Matched {
		want[m.Line] = true
	}
	stampJSON, _ := json.Marshal(stamp)
	reasonJSON, _ := json.Marshal(quarantineReason)
	members := fmt.Sprintf(`"quarantined":%s,"quarantine_reason":%s`, stampJSON, reasonJSON)
	var b bytes.Buffer
	b.Grow(len(in) + 256*len(want))
	for i, line := range bytes.Split(in, []byte("\n")) {
		if i > 0 {
			b.WriteByte('\n')
		}
		if !want[i] {
			b.Write(line)
			continue
		}
		out, err := spliceMembers(line, members)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		b.Write(out)
	}
	return b.Bytes(), nil
}

// oracleCommitted refuses -apply unless the oracle is a TRACKED, CLEAN file
// in a git repository, so the pre-state is recoverable from history (the
// .bak precedent is kept beside it too). A sweep appends to this file
// uncommitted between runs — the operator commits first, deliberately.
func oracleCommitted(path string) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if _, stderr, err := gitOut(dir, 30, "ls-files", "--error-unmatch", "--", base); err != nil {
		return fmt.Errorf("%s is not tracked in a git repository (%s): commit the oracle first so the pre-state is recoverable", base, firstLine(stderr, err))
	}
	out, stderr, err := gitOut(dir, 30, "status", "--porcelain", "--", base)
	if err != nil {
		return fmt.Errorf("git status: %s", firstLine(stderr, err))
	}
	if len(bytes.TrimSpace(out)) > 0 {
		return fmt.Errorf("%s has uncommitted changes (%s): commit the oracle first so the pre-state is recoverable", base, firstLine(out, nil))
	}
	return nil
}

// requarantineAfterRead is a test seam; production leaves it nil.
var requarantineAfterRead func()

// requarantineFile runs the selector and, with apply, rewrites the file via
// write-temp-then-rename. Immediately before the rename the oracle is READ
// BACK and compared to the bytes this rewrite was built from; the apply
// ABORTS on any difference, because the rename would otherwise drop whatever
// a live sweep appended in between.
//
// By IDENTITY (the bytes), not by size+mtime. A stat baseline has to be
// taken either before or after the read, and either way there is a window
// where a write is invisible to it — the ordering cannot be pinned by a test
// because it is the window itself that differs. It also cannot see a
// same-size in-place edit, or any change on a filesystem with a coarse
// mtime. Re-reading removes the window and the ordering question together.
func requarantineFile(path, stamp string, apply bool) (quarantineReport, string, error) {
	in, err := os.ReadFile(path)
	if err != nil {
		return quarantineReport{}, "", err
	}
	// Test seam (nil in production): the concurrent-writer guard is the one
	// path here that can LOSE data, and a race is not reproducible on its
	// own. Firing an append exactly here makes both orderings observable —
	// stat-before-read aborts and keeps the appended row, stat-after-read
	// sees a baseline that already includes it and renames the row away.
	if requarantineAfterRead != nil {
		requarantineAfterRead()
	}
	if err := looksLikeOracle(in); err != nil {
		return quarantineReport{}, "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	rep := quarantineSelect(in)
	if !apply || len(rep.Matched) == 0 {
		return rep, "", nil
	}
	if err := oracleCommitted(path); err != nil {
		return rep, "", err
	}
	bak := path + ".bak-requarantine-" + strings.ReplaceAll(strings.ReplaceAll(stamp, "/", "_"), "\\", "_")
	if _, err := os.Stat(bak); err == nil {
		return rep, "", fmt.Errorf("backup %s already exists: a previous apply at this stamp left it behind; move or delete it first so this run cannot overwrite the older pre-state", filepath.Base(bak))
	}
	if err := os.WriteFile(bak, in, 0o644); err != nil {
		return rep, "", fmt.Errorf("backup: %w", err)
	}
	// From here the backup exists only to describe a COMPLETED apply: every
	// abort below removes it, so a .bak beside the oracle always means the
	// rewrite happened.
	abort := func(format string, args ...any) (quarantineReport, string, error) {
		os.Remove(path + ".tmp")
		os.Remove(bak)
		return rep, "", fmt.Errorf(format, args...)
	}
	out, err := requarantine(in, rep, stamp)
	if err != nil {
		return abort("%v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return abort("%v", err)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		return abort("re-reading %s before the rename: %v", filepath.Base(path), err)
	}
	if !bytes.Equal(cur, in) {
		return abort("%s changed while the quarantine was being prepared (%d bytes on disk vs the %d this rewrite was built from — another process is writing it): nothing applied, re-run when the oracle is quiet", filepath.Base(path), len(cur), len(in))
	}
	if err := os.Rename(tmp, path); err != nil {
		return abort("rename %s: %v", filepath.Base(tmp), err)
	}
	return rep, bak, nil
}
