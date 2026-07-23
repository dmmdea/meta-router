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
