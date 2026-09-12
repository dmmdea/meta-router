package router

import (
	"encoding/json"
	"os"
)

// Seed returns the compiled default rank table. Every rank cites its baseline
// evidence (docs/specs/2026-07-06-v3-model-capability-baseline.md) — this IS
// the deliverable data; the evidence-citation test is the contract.
//
// Since 2026-07-25 it carries the
// GATE-CLEARED policy: the B'1 mechanical-text gold-stakes floor and the
// verify-gate demotion, both measured on the V2 gold probes and cross-validated
// (router-live 1.224x always-claude full-set, p=0.032 NON-INFERIOR at 57%
// claude-window; 1.25x on the 23-task heldout split — docs/specs/
// 2026-07-23-b2-crossval-results.md in the private repo). Before that these two
// rows still held their PRE-probe guesses ("gold-set probe owed", "operator
// smoke n=2 — WEAK") while the measured tables lived only in an untracked
// override file, so any fresh clone routed on the policy the project's own gate
// had rejected: local at rank 1 for both classes, measured 0.054 pass-rate,
// scoring 0.816x with NON-INFERIORITY FALSE (audit 2026-07-25). Promotion of a
// routing-visible change requires that gate (Bible B8).
//
// 2026-09-12 — Claude 5 re-pin (masterplan queue item 3). Every claude row that
// named `claude-opus-4-8` now names `claude-opus-5` at the same effort; the
// sonnet rows stay on `claude-sonnet-5`. Each evidence string names its
// INSTRUMENT (docs/specs/2026-09-06-frontier-refresh-v5.md §4: three boards
// share the "SWE-bench Pro" name) and says when a figure is a LEGACY Opus 4.8
// measurement with no published Opus 5 replacement — a rank held on the
// model-family prior is written as such, never re-labelled as an Opus 5 number.
// Opus-vs-Sonnet within a class is a RANK decision here; the lane's quota
// state (throttle / burn-rate downshift) shifts both claude rows together, so
// the quota-dynamic choice is claude-vs-other-lane, not opus-vs-sonnet.
// Reference config for the B8 split gate becomes `claude|claude-opus-5|high`
// (the lane's best-ranked config by key order); it is measured by the
// 2026-09-12 gold probe recorded in the private repo.
func Seed() Table {
	return Table{
		HardRepo: {
			{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh", Rank: 1, Evidence: "SWE-bench Pro, vendor-aggregate instrument (llm-stats, frontier v5 §4, 2026-09-06): Opus 5 79.2 > Terra 63.4 > Sonnet 5 63.2 > Luna 62.7 > GLM-5.2 62.1; SWE-bench Verified ~96 third-party (saturated, non-separating); Opus 5 >2x Opus 4.8 on Frontier-Bench v0.1 (vendor, relative). Supersedes the Opus 4.8 SWE-V 88.6 / SWE-Pro 69.2 citation"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 2, Evidence: "SWE-bench Pro vendor-aggregate 63.2 — same instrument as rank 1 (frontier v5 §4); same-lane fallback preserves scaffold binding (baseline §0.1); $2/$10 permanent (frontier v5 §2.1)"},
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 3, Evidence: "SWE-Pro 62.1 vendor-triangulated; entelligence: Sonnet-class on identical Claude Code scaffold"},
			// Measured fallback (2026-09-02): before this row the class had only claude + glm
			// entries, so with GLM retired (config) a `--exclude claude` consult deferred with
			// "all lanes masked" - delegate-mode yielded nothing on hard-repo-classified work.
			// Ranked LAST on purpose: it changes nothing while a claude row is selectable
			// (B'2 2026-07-23 showed class-level oracle tuning does not transfer on heldout,
			// so the SWE-V ordering above stands); it only exists so exclusion/masking has a
			// measured lane to fall to. The B8 split scorecard cannot score it against the
			// seed's reference config (oracle rows carry effort=unrecorded) - recorded honestly.
			{Lane: "codex", Model: "gpt-5.6-terra", Effort: "high", Rank: 4, Evidence: "V2 gold probe trials-3 (private eval/oracle.jsonl, 2026-07-19..27): agentic-coding task-mean verifier pass codex 0.278 > claude-sonnet-5 0.164 > glm-5.2 0.133 over 12 tasks; effort unrecorded. Measured fallback for --exclude claude / masked claude+glm, never promoted above the SWE-V rows"},
		},
		TerminalBounded: {
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 1, Evidence: "tbench.ai independent #1: GPT-5.5-in-Codex-CLI 83.4 > Opus-in-Claude-Code 78.9; SURGICAL — Plus degradation 10-20x (#28879), ledger governs"},
			{Lane: "claude", Model: "claude-opus-5", Effort: "high", Rank: 2, Evidence: "Terminal-Bench 4.0 official (tbench.ai, Sep 2026, frontier v5 §4): Opus 5 on Claude Code 51.8±3.4 vs Astra/Codex 58.2, Fable 5.1 57.9; Sonnet 5 12.4 at the worst $/point on the board — Sonnet is deliberately NOT a terminal-bounded row. Supersedes the Opus 4.8 TB-2.x 78.9 citation (different instrument version)"},
		},
		Workhorse: {
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "entelligence head-to-head 25/45 == Opus-4.7 tie on identical scaffold at ~46% cost; best-harness TB 82.7 (Z.ai-run)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 2, Evidence: "Sonnet 5: Anthropic's speed+intelligence tier, $2/$10 permanent (frontier v5 §2.1); keeps volume off the binding Claude weekly only when GLM masked"},
			{Lane: "claude", Model: "claude-opus-5", Effort: "high", Rank: 3, Evidence: "quality ceiling fallback (R14: capacity is there to be used); Anthropic: 'start with Opus 5 for most workloads' (frontier v5 §2.1)"},
		},
		ManyTool: {
			{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh", Rank: 1, Evidence: "LEGACY FIGURE: Tool-Decathlon 59.9 > 55.6 > 48.2 was measured on Opus 4.8 (vendor-against-interest, Z.ai reports its own loss — baseline §1); no Opus 5 Tool-Decathlon figure published as of 2026-09-06 (frontier v5 A1). Rank held on the family prior (>2x Opus 4.8 Frontier-Bench v0.1, vendor relative) — re-verify on the next board refresh"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 2, Evidence: "Tool-Decathlon 55.6 (< Opus 59.9, > GLM 48.2; vendor-against-interest, baseline §1)"},
			// GLM deliberately ABSENT: baseline §2 'keep heterogeneous many-tool orchestration off GLM' (48.2, vendor-confessed)
		},
		MCPStructured: {
			{Lane: "claude", Model: "claude-opus-5", Effort: "high", Rank: 1, Evidence: "LEGACY FIGURE: MCP-Atlas near-parity 77.8/76.8/75.3 was measured on Opus 4.8 (Z.ai-run, baseline §1); no Opus 5 MCP-Atlas figure as of 2026-09-06 (frontier v5 A1). Parity tier retained — quota state decides via tie-break"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 1, Evidence: "MCP-Atlas 76.8 — parity tier"},
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "MCP-Atlas 75.3 — parity tier"},
		},
		DeepReasoning: {
			{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh", Rank: 1, Evidence: "HLE: Opus 5 64.7 / 54.9 on two trackers (frontier v5 A6 — the spread between trackers exceeds the gap between models; directional only). Supersedes the Opus 4.8 49.8/57.9 verified chain (baseline §1)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "xhigh", Rank: 2, Evidence: "HLE-with-tools 57.4 (Sonnet 5, vendor, baseline §1); no refreshed Sonnet 5 HLE figure as of 2026-09-06 (frontier v5 A1: the numbers surfaced at launch were Sonnet 4.6's)"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "xhigh", Rank: 3, Evidence: "HLE ordering #3; xhigh reserved tier (stet.sh effort curve: quality 12→65% low→xhigh)"},
		},
		FormalMath: {
			{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh", Rank: 1, Evidence: "LEGACY FIGURE: USAMO 96.7 vs Sonnet 79.5 was measured on Opus 4.8 (vendor, verified, baseline §1); no Opus 5 USAMO figure as of 2026-09-06 (frontier v5 A1). Rank held on the family prior (>2x Opus 4.8 Frontier-Bench v0.1, vendor relative)"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "xhigh", Rank: 2, Evidence: "USAMO 79.5 (Sonnet 5, vendor, verified, baseline §1) — fallback below the Opus lineage's 96.7 (an Opus 4.8 figure)"},
		},
		CompetitionMath: {
			{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "AIME saturated: 99.2 ≈ GPT-5.5 98.3 — per-dollar winner in a saturated tier"},
			{Lane: "codex", Model: "gpt-5.5", Effort: "high", Rank: 2, Evidence: "AIME 98.3 (saturated tier ≈ GLM-5.2 99.2; per-dollar loser, baseline §1)"},
		},
		LongContext: {
			{Lane: "claude", Model: "claude-opus-5", Effort: "high", Rank: 1, Evidence: "1M ctx default on Opus 5 and Sonnet 5 (frontier v5 §2.1). LEGACY FIGURE: MRCR 78.3@1M, GraphWalks 68.1 vs GPT-5.5 45.4 were measured on the Opus 4.8 lineage (baseline §1); no Opus 5 or Sonnet 5 1M-retrieval figure as of 2026-09-06 — prefer Opus >300K; codex ctx-masked >258K (independent, #19319)"},
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
			{Lane: "glm", Model: "glm-5.2", Effort: "low", Rank: 1, Evidence: "V2 gold probe (all trials, 2026-07-22): gold-stakes extraction — glm 5/10 = codex = claude (3-way tie at 0.500), local 1/10. Cheapest cloud takes the tie (B'1 gold-stakes floor)"},
			{Lane: "codex", Model: "gpt-5.6-terra", Effort: "low", Rank: 2, Evidence: "V2 gold probe: 5/10 tie tier"},
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "low", Rank: 3, Evidence: "V2 gold probe: 5/10 tie tier; last of the tie to protect the binding window"},
			{Lane: "local", Model: "gemma4-cascade", Effort: "", Rank: 4, Evidence: "demoted from rank 1 on the gold probe (1/10 gold-stakes extraction). Its 0.920 runtime-mechanical measurement stands — local remains the door for offload-tier work, not for gold-stakes extraction"},
		},
		DocSummarize: {
			{Lane: "local", Model: "qwythos", Effort: "", Rank: 1, Evidence: "operator-verified config, ≤32K (baseline §1)"},
			{Lane: "glm", Model: "glm-4.7", Effort: "low", Rank: 2, Evidence: "cheap overflow when >32K or local deferred"},
		},
		VerifyGate: {
			{Lane: "claude", Model: "claude-sonnet-5", Effort: "medium", Rank: 1, Evidence: "V2 gold-set probe 2026-07-19 (the probe this entry OWED): local qwythos 0/12 on gold-level adversarial review; claude-sonnet 5/12 best"},
			{Lane: "codex", Model: "gpt-5.6-terra", Effort: "medium", Rank: 2, Evidence: "V2 gold-set probe: codex 3/12 on gold review — second measured tier"},
			{Lane: "local", Model: "qwythos", Effort: "", Rank: 3, Evidence: "demoted from rank 1 by its own owed probe (0/12); retained as the cheap gate for runtime-tier checks (baseline §2: local may GATE, never RE-LABEL)"},
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
	t, _, _ := LoadChecked(path)
	return t
}

// Provenance of the table a decision was made with.
const (
	TableSeed     = "seed"     // the compiled, gate-cleared default
	TableOverride = "override" // an operator file on disk
)

// LoadChecked is Load plus PROVENANCE and a warning. A silent fail-open to Seed
// made two machines route on different policies for weeks with nothing to see it
// (audit 2026-07-25: the laptop had no override file and scored 0.816x vs the
// desktop's 1.224x). Missing is normal and silent — the seed IS the gate-cleared
// policy. CORRUPT is not: the operator meant to override and the file is
// unusable, so it warns.
func LoadChecked(path string) (t Table, provenance, warn string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Seed(), TableSeed, "" // absent override: the seed is the policy
	}
	var got Table
	if err := json.Unmarshal(b, &got); err != nil {
		return Seed(), TableSeed, "rank-table override unreadable, routing on the compiled seed: " + err.Error()
	}
	if len(got) == 0 {
		return Seed(), TableSeed, "rank-table override is empty, routing on the compiled seed"
	}
	return got, TableOverride, ""
}
