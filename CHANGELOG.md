# Changelog

All notable changes to `meta-router` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

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
