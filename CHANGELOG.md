# Changelog

All notable changes to `meta-router` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

## [0.1.0] — 2026-06-28

### Added
- Initial public release.
- **Skill-awareness surfacer**: a `UserPromptSubmit` hook (`mr-hook`) that retrieves the most relevant installed skills per prompt (hybrid **BM25 + EmbeddingGemma + Reciprocal Rank Fusion** over a locally-built index) and injects them as `additionalContext`.
- `mr-index` builder with hash-diff incremental refresh; `mr-eval` retrieval benchmark (recall@k / MRR).
- **Offload nudge**: detects mechanical text work and suggests free local offload tools.
- Fully **local** (reuses a warm local embedder), **fail-open** (≤300 ms deadline, BM25 fallback, always exits 0), and privacy-preserving (usage log stores hashes only).
