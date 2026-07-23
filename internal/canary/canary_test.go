package canary

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
	re := regexp.MustCompile(`(?i)Getenv\("[^"]*API_KEY[^"]*"\)|"x-api-key"`)
	hits, err := ScanForbidden(files, re)
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
		t.Fatalf("go list: %v", err)
	}
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
}
