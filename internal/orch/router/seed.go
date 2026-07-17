package router

import (
	"encoding/json"
	"os"
)

// Seed returns the compiled default rank table. Every rank cites its baseline
// evidence (docs/specs/2026-07-06-v3-model-capability-baseline.md) — this IS
// the deliverable data; the evidence-citation test is the contract.
func Seed() Table {
	return Table{
		HardRepo: {
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "xhigh", Rank: 1, Evidence: "vals.ai independent same-harness SWE-V: Opus 88.6 > GPT-5.5 82.6; SWE-Pro vendor-triangulated 69.2>62.1>58.6 (baseline §1)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 2, Evidence: "SWE-Pro ordering #2 tier; same-lane fallback preserves scaffold binding (baseline §0.1)"},
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 3, Evidence: "SWE-Pro 62.1 vendor-triangulated; entelligence: Sonnet-class on identical Claude Code scaffold"},
		},
		TerminalBounded: {
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 1, Evidence: "tbench.ai independent #1: GPT-5.5-in-Codex-CLI 83.4 > Opus-in-Claude-Code 78.9; SURGICAL — Plus degradation 10-20x (#28879), ledger governs"},
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "high", Rank: 2, Evidence: "tbench.ai 78.9 same-harness independent"},
		},
		Workhorse: {
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "entelligence head-to-head 25/45 == Opus-4.7 tie on identical scaffold at ~46% cost; best-harness TB 82.7 (Z.ai-run)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 2, Evidence: "Sonnet-class peer; keeps volume off the binding Claude weekly only when GLM masked"},
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "high", Rank: 3, Evidence: "quality ceiling fallback (R14: capacity is there to be used)"},
		},
		ManyTool: {
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "xhigh", Rank: 1, Evidence: "Tool-Decathlon 59.9 > 55.6 > 48.2, vendor-against-interest (Z.ai reports its own loss) — most trustworthy tool-use discriminator"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 2, Evidence: "Tool-Decathlon 55.6 (< Opus 59.9, > GLM 48.2; vendor-against-interest, baseline §1)"},
			// GLM deliberately ABSENT: baseline §2 'keep heterogeneous many-tool orchestration off GLM' (48.2, vendor-confessed)
		},
		MCPStructured: {
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "high", Rank: 1, Evidence: "MCP-Atlas near-parity 77.8/76.8/75.3 (Z.ai-run) — quota state decides via tie-break"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 1, Evidence: "MCP-Atlas 76.8 — parity tier"},
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "MCP-Atlas 75.3 — parity tier"},
		},
		DeepReasoning: {
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "xhigh", Rank: 1, Evidence: "HLE 49.8/57.9 verified chain (baseline §1)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "xhigh", Rank: 2, Evidence: "HLE-with-tools 57.4 nearly matches Opus"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "xhigh", Rank: 3, Evidence: "HLE ordering #3; xhigh reserved tier (stet.sh effort curve: quality 12→65% low→xhigh)"},
		},
		FormalMath: {
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "xhigh", Rank: 1, Evidence: "USAMO 96.7 vs Sonnet 79.5 (vendor, verified)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "xhigh", Rank: 2, Evidence: "USAMO 79.5 (Sonnet-5 fallback below Opus 96.7; vendor, verified, baseline §1)"},
		},
		CompetitionMath: {
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "AIME saturated: 99.2 ≈ GPT-5.5 98.3 — per-dollar winner in a saturated tier"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 2, Evidence: "AIME 98.3 (saturated tier ≈ GLM-5.2 99.2; per-dollar loser, baseline §1)"},
		},
		LongContext: {
			{Lane: "claude", Model: "claude-opus-4-8", Effort: "high", Rank: 1, Evidence: "MRCR 78.3@1M, GraphWalks 68.1 vs GPT-5.5 45.4; Sonnet-5 1M retrieval UNPUBLISHED — prefer Opus >300K; codex ctx-masked >258K (independent, #19319)"},
		},
		LatencyIter: {
			{Lane: "glm", Model: "glm-5.2", Effort: "medium", Rank: 1, Evidence: "Artificial Analysis independent: 206.8 tok/s, TTFT 1.42s vs Opus 59.0/40.3s — 3.5x throughput at ~1/6 price; local masked (21s swap TTFT, baseline §2)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "medium", Rank: 2, Evidence: "unmeasured fallback — no independent latency figure; recorded as prior-free"},
		},
		CheapToolLoops: {
			{Lane: "glm", Model: "glm-4.7", Effort: "high", Rank: 1, Evidence: "THE measured R14a cheap-tier exception: SWE-rebench 58.7 independent, tau2-telecom 95.9, always-1x quota; NOT a general worker (agg 45.3 vs 81)"},
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 2, Evidence: "stronger sibling when the 4.7 case is marginal (R14a default-up)"},
		},
		MechanicalText: {
			{Lane: "local", Model: "gemma4-cascade", Effort: "", Rank: 1, Evidence: "operator-measured (outranks vendor numbers): 0.920/1.000 vs gold, 0.86s/call, 320/320 — cheapest tier WINS this class"},
			{Lane: "glm", Model: "glm-4.7", Effort: "low", Rank: 2, Evidence: "haiku-pin at always-1x quota (fact refresh §3)"},
		},
		DocSummarize: {
			{Lane: "local", Model: "qwythos", Effort: "", Rank: 1, Evidence: "operator-verified config, ≤32K (baseline §1)"},
			{Lane: "glm", Model: "glm-4.7", Effort: "low", Rank: 2, Evidence: "cheap overflow when >32K or local deferred"},
		},
		VerifyGate: {
			{Lane: "local", Model: "qwythos", Effort: "", Rank: 1, Evidence: "operator smoke n=2 — WEAK, gold-set probe owed; local may GATE, never RE-LABEL (baseline §2)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "medium", Rank: 2, Evidence: "cloud gate fallback; different-lane-than-worker discipline arrives with slice-3 strategies"},
		},
		HardCaseReclaim: {
			{Lane: "local", Model: "qwythos-think", Effort: "", Rank: 1, Evidence: "operator-measured strictly non-negative, 88→100% coverage (baseline §1)"},
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 2, Evidence: "cloud reclaim = the workhorse prior"},
		},
	}
}

// Load reads an operator rank-table override from path; a missing or corrupt
// file fails open to Seed() (fuses pattern; config-not-code). An empty/partial
// table on disk is honored verbatim — the operator owns the override file.
func Load(path string) Table {
	b, err := os.ReadFile(path)
	if err != nil {
		return Seed()
	}
	var t Table
	if err := json.Unmarshal(b, &t); err != nil || len(t) == 0 {
		return Seed()
	}
	return t
}
