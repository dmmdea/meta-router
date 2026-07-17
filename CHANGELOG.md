# Changelog

All notable changes to `meta-router` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

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
