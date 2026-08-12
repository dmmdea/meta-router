package canary

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/router"
)

// oraclePath resolves the replay oracle. It lives in the PRIVATE repo, so a
// public clone legitimately has none — that is a skip, not a failure. MR_ORACLE
// overrides; otherwise look for the private sibling checkout.
//
// Deliberately NOT the liveOrSkip TCP-dial shape (ledger row 93): that pattern
// skips for reasons unrelated to the thing under test. Here the skip condition
// is exactly "the evidence file is absent", and it says so.
func oraclePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("MR_ORACLE"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("MR_ORACLE=%s is set but unreadable: %v", p, err)
		}
		return p
	}
	root, err := RepoRoot()
	if err != nil {
		t.Skipf("repo root unresolved: %v", err)
	}
	p := filepath.Join(root, "..", "meta-router", "eval", "oracle.jsonl")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("oracle not present (it lives in the private repo; set MR_ORACLE to point at it): %s", p)
	}
	return p
}

// loadOracleModels returns the set of "lane|model" pairs the oracle actually
// observed. A DEFERRED row is a hole, not an observation (B6) — counting it
// would let a lane that refused every hard task look measured.
func loadOracleModels(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(oraclePath(t))
	if err != nil {
		t.Fatalf("open oracle: %v", err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r struct {
			Lane         string `json:"lane"`
			Model        string `json:"model"`
			OutcomeClass string `json:"outcome_class"`
		}
		if json.Unmarshal(line, &r) != nil || r.Lane == "" {
			continue
		}
		if r.OutcomeClass == "deferred" {
			continue
		}
		out[r.Lane+"|"+r.Model] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read oracle: %v", err)
	}
	return out
}

// rankedModelsWithoutEvidence lists every (lane, model) the rank table ranks
// that the oracle never observed, deduped and sorted so the failure message is
// deterministic.
func rankedModelsWithoutEvidence(tbl router.Table, observed map[string]bool) []string {
	seen, missing := map[string]bool{}, []string{}
	for _, entries := range tbl {
		for _, e := range entries {
			k := e.Lane + "|" + e.Model
			if seen[k] {
				continue
			}
			seen[k] = true
			if !observed[k] {
				missing = append(missing, k)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// B15: a ranked model with no evidence is a recommendation with nothing
// behind it. Model level, not full cell — see the Bible entry for why.
func TestCanaryB15RankedModelsHaveEvidence(t *testing.T) {
	uncovered := rankedModelsWithoutEvidence(router.Seed(), loadOracleModels(t))
	if len(uncovered) > 0 {
		t.Fatalf("B15 violated — the rank table ranks (lane,model) pairs with no oracle evidence: %v\n"+
			"Either measure them (mr-goldreplay) or stop ranking them. Pooling a sibling model's history is not evidence (B6).", uncovered)
	}
}
