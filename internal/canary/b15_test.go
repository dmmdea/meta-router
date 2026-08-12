package canary

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
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
			Dispatched   bool   `json:"dispatched"`
		}
		if json.Unmarshal(line, &r) != nil || r.Lane == "" {
			continue
		}
		// A hole is not evidence. Skipping only "deferred" let rows where
		// NOTHING RAN count as measurement: an entire replay against a
		// missing/stale binary writes `dispatched:false, outcome_class:error`
		// for every cell, and B15 turned GREEN for a model that was never
		// dispatched once (review 2026-08-12, reproduced on a synthetic
		// oracle). Evidence means the dispatch happened and returned a verdict.
		if !r.Dispatched {
			continue
		}
		switch r.OutcomeClass {
		case "deferred", "error", "spawn_error", "config_error", "parse_error", "api_error":
			continue
		}
		if strings.HasPrefix(r.OutcomeClass, "exit-") {
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
// knownUncovered are the pairs the pending re-baseline is scheduled to close.
// They are ACKNOWLEDGED, not excused: the canary still reports them loudly on
// every run, and a pair appearing here that is NOT in this list fails.
//
// Why an allowlist rather than a red test: B15 shipped knowingly failing, and
// a permanently-red suite gates nothing — a genuine regression in any package
// hides behind the known failure (review 2026-08-12). Shrink this list as the
// re-baseline measures each pair; when it is empty, delete it.
var knownUncovered = map[string]bool{
	"claude|claude-opus-4-8": true,
	"codex|gpt-5.5":          true,
	"glm|glm-4.7":            true,
	"local|qwythos":          true,
	"local|qwythos-think":    true,
}

func TestCanaryB15RankedModelsHaveEvidence(t *testing.T) {
	// The ACTIVE table, not the compiled default: an override that ranks a
	// different model per lane is what the router actually dispatches, and
	// gating the seed would pass while the real policy ranks unmeasured
	// models (review 2026-08-12).
	tbl, provenance, warn := router.LoadChecked(statepaths.RankTable())
	if warn != "" {
		t.Logf("rank table: %s", warn)
	}
	t.Logf("B15 gating the %s rank table", provenance)
	uncovered := rankedModelsWithoutEvidence(tbl, loadOracleModels(t))
	var unexpected []string
	for _, k := range uncovered {
		if !knownUncovered[k] {
			unexpected = append(unexpected, k)
		}
	}
	if len(uncovered) > 0 {
		t.Logf("B15: %d ranked (lane,model) pair(s) still have no oracle evidence: %v\n"+
			"These are the re-baseline's work list. A ranked model nobody measured is a recommendation with nothing behind it.", len(uncovered), uncovered)
	}
	if len(unexpected) > 0 {
		t.Fatalf("B15 violated — NEW ranked (lane,model) pair(s) with no evidence: %v\n"+
			"Either measure them (mr-goldreplay) or stop ranking them. Pooling a sibling model's history is not evidence (B6).", unexpected)
	}
	// A pair that got measured must leave the allowlist, or the list rots into
	// a permanent excuse nobody rechecks.
	observed := loadOracleModels(t)
	var stale []string
	for k := range knownUncovered {
		if observed[k] {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("B15 bookkeeping — these pairs now HAVE evidence and must be removed from knownUncovered: %v", stale)
	}
}
