# Changelog

All notable changes to `meta-router` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

## [0.9.0] — 2026-07-23

### Added
- **W8 — process port scaffold:** `ROUTER_BIBLE.md` (invariants B1–B12, each with a `verify:` pointer) enforced by the new `internal/canary` suite: B1 no-API-key-auth source scan, B2 router-package purity (`go list -deps` — no net/exec in the hot path), B3 margin-floor pin (0.15), B11 version parity (VERSION == orchestrator version var — this canary immediately caught the deployed binary reporting `0.4.0-slice4`), B12 complexity ratchet (`docs/complexity-budget.json`, 15500 over 14340 measured), Bible hash concept-gate (`docs/bible.sum` + CONCEPT-CHANGE protocol), verify-pointer resolution, and adjudication-ledger structure. Review protocol codified: adjudication ledger + declined-findings feed-forward + reviewer liveness floor (`docs/reviews/`).

### Fixed
- `cmd/mr-orchestrate` version var un-drifted (`0.4.0-slice4` → tracks VERSION; B11 pins it forever).
- Review round 1 (2 MAJORs, 4 minors, 4 nits — all adjudicated in `docs/reviews/adjudication-ledger.md`): B1 pattern extended to `LookupEnv`/`APIKEY` and hoisted to a single shared definition; B11 gained its CHANGELOG leg; ledger canary hardened (TrimSpace, no-pipe rule); vendor/ skipped; `go list` stderr surfaced; outside-marker invariant bullets now fatal.
- CONCEPT-CHANGE: B5 verify pointer repointed to `internal/orch/router/fault_test.go` (router_test.go has no override coverage); B11 scope documented (deployed orchestrator binary only — sibling dev tools versioned independently).

## [0.8.0] — 2026-07-23

### Added
- **B'2 — cross-validated scorecard** (`mr-scorecard -split` + `policyeval.ClassBest/ByClass/ClassCoverage`): derive a per-class best-lane policy on the goldset's TUNING split only (task-mean objective — the same unweighted mean `Evaluate` scores; per-class per-lane coverage emitted so hole-driven picks are visible), score every policy on the HELDOUT split. The per-task oracle row is marked `in_sample` with its non-inferiority verdict suppressed — a ceiling, never a deployable claim. Goldset `split` labels are hard-validated (exit 2 on a typo — silent tuning contamination is the failure mode).
- Sign-flip permutation is EXACT up to n≤24 (was n≤20), covering the heldout n=23 so split verdicts never sit in the Monte-Carlo seed-luck regime.

### Changed
- **router-live probes now run in a NEUTRAL quota state by default** (isolated temp `MR_ORCH_STATE`; weather zeroed, POLICY INPUTS preserved — rank-table override, config, fuses — with loud failure if a policy file is unreadable): the scorecard answers the policy question, not today's window weather (observed degenerations: exhausted windows → router-live ≡ always-claude; bare temp dir → Seed table measured instead of the tuned one). `-live-quota` restores real-state probing. Probes always pass `-no-receipt` (they must not pollute the delegation-coverage numerator) and only the evaluated tasks are probed.
- Total pick order in oracle/class derivations: unknown lanes cost MAX, equal-cost ties break lexically — map iteration can never decide a pick.

## [0.7.0] — 2026-07-22

### Added
- **E2 — spend-down** (`internal/orch/spenddown`): a premium window measured under-utilized near its reset earns a bounded, ramped rank BOOST (rank-delta, never scalar) for explicitly batch-tagged, already-queued work — never interactive consults. Q2-locked refinements all present: completion-fit start gate (expected duration must end before reset + buffer; unknown never fits), forecast trigger on the PROJECTED end-of-window unused fraction (anchored at the newest windowed trace read, never instantaneous or mean-lagged), lane-specific per-bucket ResetsAt, hysteresis (arm <25% / disarm >35% averaged used) with a cooldown-gated one-level-per-interval ramp bounded at 2, and immediate drops. Hard guards (several from the adversarial review): ARMING requires a live windowed trace read (≥2 same-epoch samples) — no-data can hold or drop the latch but never start a boost; the latch is EPOCH-SCOPED (a window reset zeroes it, so a fresh window re-ramps from 1, never inherits a full-level blast); estimate-sourced buckets never arm; a lane under an active E1 downshift or any non-open admission state never boosts AND its latch freezes (no background ramp); local never boosts; dry-run/introspection consults preview the boost without persisting. All thresholds are config priors (`spend_down_*` incl. `spend_down_avg_window_min`, `spend_down_off` kill-switch) awaiting live-trace calibration; the latch persists at `~/.meta-router/orchestrate/spend-down.json` (unique-temp atomic writes).
- Surfaces: `route`/`run` gain `-batch` + `-est-minutes` (CLI, fractional ok) and `batch` + `est_minutes` (MCP tools); route JSON adds additive `spend_down_boost`; the winner's `reason` names the applied boost; `status`/`quota_status` show the read-only armed latch per lane (`spend_down`); dispatch receipts record `batch` + `spend_down_boost` so boost-influenced decisions stay countable (the calibration substrate).

## [0.6.0] — 2026-07-22

### Added
- **V7 — the equal-budget strategy-template promotion gate** (`internal/promotion` + `mr-promote`): a template earns default status ONLY via the conjunctive rule on paired template-vs-solo runs at matched token budget — paired BCa CI lower bound > 0 AND sign-flip permutation p < 0.05. Budget-skewed pairs (>25% divergence) are excluded and counted; at small n the gate refuses by arithmetic (5/5 perfect wins still refuse at p=0.0625). Until promoted, templates remain manual `--strategy` seams.
- **V3+V5 — policy evaluator + WF@Q scorecard** (`internal/policyeval` + `mr-scorecard`): exact-lookup policy evaluation over the oracle replay table, oracle-best/frontier/regret/RCI, BCa bootstrap + sign-flip statistics, pre-registered non-inferiority verdict; `router-live` measured via the REAL shipped classifier on raw prompts.
- **V2 — the oracle replay runner** (`mr-goldreplay`): every gold task × lane × trial through `mr-orchestrate run` (ledger-metered, receipted), verified by the pure engine or `mr-goldverify`; resumable by cell; deferred rows are holes that refill when a window reopens.

### Fixed
- Admission: an expired quota window never gates (stale percentages are history, not pressure).
- Replay chain robustness: worktree diff harvest (raw bytes), printed-diff decode/truncate/`--recount`, tool-enabled headless claude agents, codex model ids, notional ceiling flag.

## [0.5.0] — 2026-07-17

### Changed
- **This repo is now the PRIMARY development repo** (operator-agnostic single-source inversion): all product code lives here, operator-neutral at rest; personal gold sets and operator config live outside the repo. All merges via PR.

### Added
- **The v3 multi-lane orchestrator** (`mr-orchestrate` + `internal/orch`, 20 packages): routes tasks across claude / codex / GLM / local lanes with a quota ledger, admission gates, burn-rate pacing, per-lane error taxonomies, dispatch receipts, strategy templates, and an MCP surface.
- **The eval substrate**: `internal/oracle` (V0 null/trivial audit gate), `internal/goldtask` (routing gold-set schema + pure verifier engine), `cmd/mr-goldverify` (execution verifier: worktree at parent → apply candidate diff → held-out tests), `internal/verifierceiling` + `cmd/mr-verifier` (local-verifier ceiling: AURC/AUGRC + defer-discounted effective ceiling), with generic verifier corpora. Bring your own gold set via `-goldset` — gold-set-dependent tests skip when none is present.
- `mr-hook` **quota+route hint** restored (`-quota-hint`, ledger-direct, fail-open) now that its orchestrator dependency ships here.
- `scripts/mr-statusline-tee.js` — statusline tee that feeds the quota trace.

## [0.4.0] — 2026-07-15

### Added
- **Machine-agnostic embedder endpoint resolution** (`internal/retrievers/endpoint.go`): the `-endpoint` flag now defaults to empty and resolves per machine — `$MR_EMBED_ENDPOINT` first, then `~/.meta-router/endpoints.json`, then a built-in `:11436`→`:18793` failover chain probed under one shared deadline. A hardcoded per-machine port in shared config can no longer silently kill semantic surfacing on a second machine.
- `mr-eval` probes the *resolved* endpoint candidates (not the raw flag), so an empty flag no longer reports the embedder down and silently scores BM25 only.
- Index entry-rename handling (`internal/index/rename.go`): a skill whose ID changes is treated as remove+add, never a stale duplicate.

### Fixed
- Shared failover deadline, dimension-guard, and error-classification hardening in the embed client (adversarial-review fixes riding the endpoint-resolution change).

## [0.3.0] — 2026-07-04

### Added
- First public release. v0.3.0 = v1 (skill-awareness surfacer) + v2 (offload nudge) shipped; v3 (headless orchestrator + quota-ledger) is the next milestone.
- **Skill-awareness surfacer**: a `UserPromptSubmit` hook (`mr-hook`) that retrieves the most relevant installed skills per prompt and injects them as `additionalContext`.
- **Plugin-pack indexing**: roots are discovered from the user skills dir *plus* every installed plugin's skills dir, persisted to `roots.json`; skills are identified by their invocable name (`<plugin>:<skill>`), with harvest hygiene (hidden dirs, installer temp clones, `node_modules` skipped; description-identical twins collapsed).
- **Embed-primary ranking** (EmbeddingGemma cosine) with a confidence gate, plus a *precision-gated* BM25 fallback when the embedder is down; the BM25+RRF hybrid remains available via `-ranker=hybrid`.
- `mr-index` builder with hash-diff incremental refresh, per-run `refresh.log` status lines, a >30% mass-removal guard (`-force` to override), and dated single-backup rotation on index overwrite.
- **Fast index loads**: a gob/float32 `index.bin` sidecar parses ~10× faster than the JSON index (JSON stays the source of truth and fallback).
- `mr-eval` retrieval benchmark (recall@k / MRR, covered-only subset reporting) and `mr-outcomes` (joins surfacings with Skill-tool invocations from `outcomes.jsonl` to report surfaced→invoked hit-rate).
- **Offload nudge**: detects mechanical text work and suggests free local offload tools.
- Fully **local** (any OpenAI-compatible `/v1/embeddings` endpoint — a native `llama-server.exe` sidecar works on Windows with no WSL dependency), **fail-open** (≤300 ms deadline, ~200 ms connect timeout, always exits 0), and privacy-preserving (usage log stores hashes only).

## [0.2.0] — 2026-06-28

### Changed
- Internal pre-release version bump correcting an erroneous 0.1.0 marker; no functional changes. (v0.3.0 above is the first published release.)
