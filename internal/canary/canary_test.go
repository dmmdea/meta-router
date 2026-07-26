package canary

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/egress"
)

// readBibleInvariants returns the normalized invariants block (CRLF→LF).
func readBibleInvariants(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "ROUTER_BIBLE.md"))
	if err != nil {
		t.Fatalf("ROUTER_BIBLE.md unreadable: %v", err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	const begin, end = "<!-- invariants:begin -->", "<!-- invariants:end -->"
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < 0 || j < i {
		t.Fatal("invariants markers missing/misordered in ROUTER_BIBLE.md")
	}
	// Invariant-styled text outside the markers would read as Bible law while
	// escaping both the hash gate and pointer checks — refuse it.
	if outside := s[:i] + s[j+len(end):]; strings.Contains(outside, "- **B") {
		t.Fatal("invariant-styled bullet ('- **B') found outside the hash-gated markers block")
	}
	return s[i+len(begin) : j]
}

// Concept gate: the invariants block hash must match docs/bible.sum.
func TestCanaryBibleHash(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(readBibleInvariants(t, root)))
	got := hex.EncodeToString(sum[:])
	wb, err := os.ReadFile(filepath.Join(root, "docs", "bible.sum"))
	if err != nil {
		t.Fatalf("docs/bible.sum unreadable (current invariants hash: %s): %v", got, err)
	}
	if want := strings.TrimSpace(string(wb)); got != want {
		t.Fatalf("CONCEPT GATE — ROUTER_BIBLE invariants changed.\nnew hash: %s\nIf intended: update docs/bible.sum to the new hash AND add a CONCEPT-CHANGE line to this version's CHANGELOG entry (see ROUTER_BIBLE.md protocol).", got)
	}
}

// Every invariant's verify: pointer must resolve — a Test func that exists,
// a path that exists, or the literal `process`.
func TestCanaryBibleVerifyPointers(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	inv := readBibleInvariants(t, root)
	re := regexp.MustCompile("verify: `([^`]+)`")
	ms := re.FindAllStringSubmatch(inv, -1)
	if len(ms) < 12 {
		t.Fatalf("expected >=12 verify pointers, found %d", len(ms))
	}
	tests, err := GoSourceFiles(root, true)
	if err != nil {
		t.Fatal(err)
	}
	var testSrc strings.Builder
	for _, f := range tests {
		if strings.HasSuffix(f, "_test.go") {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			testSrc.Write(b)
		}
	}
	for _, m := range ms {
		ptr := m[1]
		switch {
		case ptr == "process":
		case strings.HasPrefix(ptr, "Test"):
			if !strings.Contains(testSrc.String(), "func "+ptr+"(") {
				t.Errorf("verify pointer %q: no such test func", ptr)
			}
		default:
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ptr))); err != nil {
				t.Errorf("verify pointer %q: path does not exist", ptr)
			}
		}
	}
}

// B1 — subscription-auth only: no source reads an *_API_KEY env var or sets an
// x-api-key header. (DG-2's free lane class, when it lands, amends this canary
// explicitly — a CONCEPT-CHANGE, never a quiet edit.)
func TestCanaryB1NoAPIKeyAuth(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := GoSourceFiles(root, false)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := ScanForbidden(files, B1Forbidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("B1 violated — API-key auth pattern in source:\n%s\nSee ROUTER_BIBLE.md B1; a free-lane exception is a CONCEPT-CHANGE.", strings.Join(hits, "\n"))
	}
}

// B2 — the routing hot path is deterministic and LLM-free: the router package
// dependency closure must contain no network or subprocess capability.
func TestCanaryB2RouterPurity(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "./internal/orch/router")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			t.Fatalf("go list: %v: %s", err, xe.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	// Anti-drift granularity: catches an accidental net/http/exec import.
	// Capability reachable via package os (StartProcess) or raw syscall is out
	// of scope — those never arrive by accident.
	forbidden := map[string]bool{"net": true, "net/http": true, "os/exec": true}
	for _, dep := range strings.Fields(string(out)) {
		if forbidden[dep] {
			t.Fatalf("B2 violated — router hot path depends on %q (must stay network- and subprocess-free)", dep)
		}
	}
}

// B3 — the non-inferiority margin is 0.15, floored, never widened: the
// scorecard's flag default is pinned. Widening the margin weakens every
// promotion verdict retroactively — that is a CONCEPT-CHANGE.
func TestCanaryB3MarginFloor(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "cmd", "mr-scorecard", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`flag\.Float64\("margin", 0\.15,`).Match(b) {
		t.Fatal("B3 violated — mr-scorecard margin default is no longer the pre-registered 0.15")
	}
}

// B11 — version parity: the VERSION file and the mr-orchestrate version var
// must agree, or a deployed binary lies about what it is (observed 2026-07-23:
// binary said 0.4.0-slice4 while VERSION said 0.8.0).
func TestCanaryB11VersionParity(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	vb, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(vb))
	mb, err := os.ReadFile(filepath.Join(root, "cmd", "mr-orchestrate", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`var version = "([^"]+)"`).FindSubmatch(mb)
	if m == nil {
		t.Fatal("B11: version var not found in cmd/mr-orchestrate/main.go")
	}
	if got := string(m[1]); got != version {
		t.Fatalf("B11 violated — VERSION=%s but mr-orchestrate version var=%s (bump both in the same commit)", version, got)
	}
	// Third leg of B11: the CHANGELOG's top entry must be this version — a
	// bump with no changelog (or a stale top entry) is the same drift class.
	cb, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	cm := regexp.MustCompile(`(?m)^## \[([^\]]+)\]`).FindSubmatch(cb)
	if cm == nil {
		t.Fatal("B11: no '## [x.y.z]' heading found in CHANGELOG.md")
	}
	if got := string(cm[1]); got != version {
		t.Fatalf("B11 violated — VERSION=%s but CHANGELOG top entry is [%s] (all three legs move together)", version, got)
	}
}

// B12 — complexity ratchet: total non-test Go LOC must stay under the
// committed budget. Raising the budget is a conscious, diff-visible act.
func TestCanaryB12ComplexityRatchet(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := GoSourceFiles(root, false)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		total += strings.Count(string(b), "\n")
	}
	bb, err := os.ReadFile(filepath.Join(root, "docs", "complexity-budget.json"))
	if err != nil {
		t.Fatalf("B12: docs/complexity-budget.json unreadable (measured LOC right now: %d): %v", total, err)
	}
	var budget struct {
		MaxGoLOC int `json:"max_go_loc"`
	}
	if err := json.Unmarshal(bb, &budget); err != nil || budget.MaxGoLOC <= 0 {
		t.Fatalf("B12: bad budget file: %v", err)
	}
	if total > budget.MaxGoLOC {
		t.Fatalf("B12 violated — %d non-test Go LOC exceeds budget %d; raise docs/complexity-budget.json consciously in this PR if the growth is justified", total, budget.MaxGoLOC)
	}
}

// The adjudication ledger stays machine-checkable: 7 columns, valid verdicts,
// ISO dates. A malformed append is caught at commit time, not discovery time.
func TestCanaryAdjudicationLedger(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "docs", "reviews", "adjudication-ledger.md"))
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]bool{"fixed": true, "declined": true, "deferred": true}
	dateRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	rows := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line) // an indented row must not evade validation
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") || strings.HasPrefix(line, "| date") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 7 {
			t.Errorf("ledger row needs 7 cells, got %d: %s", len(cells), line)
			continue
		}
		rows++
		if d := strings.TrimSpace(cells[0]); !dateRe.MatchString(d) {
			t.Errorf("bad date %q in: %s", d, line)
		}
		if v := strings.TrimSpace(cells[5]); !verdicts[v] {
			t.Errorf("bad verdict %q in: %s (want fixed|declined|deferred)", v, line)
		}
	}
	if rows == 0 {
		t.Fatal("ledger has no data rows")
	}
}

// B13 — every process this system spawns gets a SCRUBBED environment. R4 was
// first "fixed" at two of five spawn sites; probe/verify/locallane still handed
// children the ambient env, and probe+verify make LIVE billable calls that
// write no receipt, so unscrubbed spend there is invisible (review 2026-07-25).
// A grep-level canary is crude, but the alternative — remembering — already
// failed once.
func TestCanaryB13EverySpawnScrubsEnv(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := GoSourceFiles(root, false)
	if err != nil {
		t.Fatal(err)
	}
	spawn := regexp.MustCompile(`exec\.Command(Context)?\(`)
	scrubCall := regexp.MustCompile(`childenv\.Scrub\(`)
	// Exemptions are an explicit ALLOWLIST, not a content heuristic. The first
	// version excused any file mentioning "taskkill"/"git"/"go" as a
	// "process-control helper" — and claudelane, codexlane and locallane all kill
	// their child on timeout, so the three most important lane spawn sites were
	// silently exempt: deleting their scrub still PASSED (mutation-tested
	// 2026-07-25). An escape hatch keyed on the condition under test is not an
	// escape hatch, it is a hole. Adding a file here must be a conscious act with
	// a stated reason.
	exempt := map[string]string{
		"cmd/mr-goldverify/main.go": "spawns only git, go and the gold task's own verify command — no model lane, so R10 does not apply",
	}
	for _, f := range files {
		rel := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(f), filepath.ToSlash(root)+"/"))
		if strings.Contains(rel, "internal/canary/") {
			continue // this file's own regexes
		}
		if _, ok := exempt[rel]; ok {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments stripped: a comment MENTIONING childenv.Scrub used to satisfy
		// this check, so deleting the call still passed (mutation-tested).
		src := StripGoComments(string(b))
		if !spawn.MatchString(src) {
			continue
		}
		if !scrubCall.MatchString(src) {
			t.Errorf("B13 violated — %s spawns a process but never calls childenv.Scrub; an ambient ANTHROPIC_API_KEY would redirect it to metered spend (R10). If this file genuinely spawns no model lane, add it to the exempt map with a reason.", rel)
		}
	}
}

// B14 — every third-party lane adapter reaches the egress gate.
//
// The gate's PREDICATE is lane-generic, but enforcement is not inherited: an
// adapter that never calls egress.Plan is simply ungated, and the CHANGELOG for
// v0.15.0 originally claimed the approved free lanes "inherit" the gate when
// exactly one adapter (glm) called it. This canary turns that claim into a
// mechanical one — seating groq/cloudflare/gemini as a run<Lane>Lane dispatcher
// without wiring the gate fails here rather than exporting a client checkout.
//
// The lane set comes from egress.ThirdPartyLanes, so the canary cannot drift
// from the gate it polices.
func TestCanaryB14ThirdPartyLanesAreGated(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := GoSourceFiles(root, false)
	if err != nil {
		t.Fatal(err)
	}
	// Collect each lane's dispatcher body: `func run<Lane>Lane(` up to the next
	// top-level func. fn keeps the SOURCE spelling (runGLMLane, not runglmLane)
	// so a failure names something greppable.
	bodies, fn := map[string]string{}, map[string]string{}
	for _, f := range files {
		if strings.Contains(filepath.ToSlash(f), "/internal/canary/") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// StripGoComments also normalizes CRLF→LF, which the `\n}\n` body
		// terminator below needs: a fresh `git clone` on Windows checks these
		// files out with CRLF and the match would silently never fire. Both this
		// and the comment stripping are mutation-tested, not assumed.
		src := StripGoComments(string(b))
		for _, lane := range egress.ThirdPartyLanes {
			re := regexp.MustCompile(`(?is)\nfunc (run` + regexp.QuoteMeta(lane) + `Lane)\(.*?(\n}\n)`)
			if m := re.FindStringSubmatch(src); m != nil {
				bodies[lane], fn[lane] = m[0], m[1]
			}
		}
	}
	if len(bodies) == 0 {
		t.Fatal("B14 found NO run<Lane>Lane dispatcher for any third-party lane — either the naming convention changed (fix this canary) or the gate now polices nothing")
	}
	if _, ok := bodies["glm"]; !ok {
		t.Error("B14: glm is the shipped third-party lane and its dispatcher was not found — the convention this canary keys on has changed and the check has gone inert")
	}
	planCall := regexp.MustCompile(`egress\.Plan\(`)
	for lane, body := range bodies {
		if !planCall.MatchString(body) {
			t.Errorf("B14 violated — %s (lane %q) dispatches a THIRD-PARTY lane but never calls egress.Plan; prompt and repo context would leave with no data-boundary decision and no receipt entry", fn[lane], lane)
		}
	}
}
