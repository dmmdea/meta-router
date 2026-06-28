<div align="center">

# meta-router

**A capability router for Claude Code — surfaces the right skills, on the right prompt, fully local.**

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/dmmdea/meta-router.svg)](https://pkg.go.dev/github.com/dmmdea/meta-router)
[![Go Report Card](https://goreportcard.com/badge/github.com/dmmdea/meta-router)](https://goreportcard.com/report/github.com/dmmdea/meta-router)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

## What & why

Claude Code can install far more skills than it can keep in context. The skill list is budgeted to a small fraction of the context window, with a per-skill description cap, and **once you pass roughly 30–50 skills the least-recently-used ones get silently dropped** — so your long-tail skills exist on disk but never get surfaced to the model.

**meta-router** is a `UserPromptSubmit` hook that fixes this with *retrieve-before-expose*: for every prompt, it retrieves the few most relevant installed skills using a hybrid lexical + semantic search over a locally built index, and injects them as `additionalContext`. The model sees exactly the skills that matter for *this* task, even if you have hundreds installed. It runs entirely on your machine — no API keys, no data leaves the box — by reusing a local embedding endpoint you already have. And it is **strictly additive and fail-open**: on any error, timeout, or cold embedder, it surfaces nothing and exits cleanly, so it can never block or break a prompt.

## Features

- **Fixes the dropped-long-tail problem** — relevant skills get surfaced regardless of how many you have installed.
- **Hybrid retrieval** — BM25 (lexical) + EmbeddingGemma cosine (semantic), fused with Reciprocal Rank Fusion (RRF) for robust ranking.
- **Confidence-gated** — only surfaces when the top semantic match clears a cosine threshold, so quiet prompts stay quiet (no noise).
- **Fully local & private** — embeddings run against a local OpenAI-compatible endpoint; prompts are never sent to any cloud, and the usage log stores only a hash + length, never raw text.
- **Fail-open by design** — a 300 ms hard deadline, BM25 fallback when the embedder is cold, and an unconditional clean exit. It cannot wedge your prompt.
- **Cheap incremental index** — a hash-diff `refresh` re-embeds only the skills whose content changed, fast enough to run on every session start.
- **Single static Go binaries** — no runtime, no daemon of its own.
- **Bonus offload nudge** — detects mechanical text tasks (summarize / classify / extract / triage over a pasted block) and gently points at free local tools instead of burning cloud context.

## Quickstart

You need **Go 1.26+** and a local OpenAI-compatible embedding endpoint serving an `embeddinggemma` model at `http://127.0.0.1:11436` (e.g. via [llama-swap](https://github.com/mostlygeek/llama-swap) or any server exposing `/v1/embeddings`).

```bash
# 1. Clone and build the binaries
git clone https://github.com/dmmdea/meta-router.git
cd meta-router
go build -o bin/mr-hook  ./cmd/mr-hook
go build -o bin/mr-index ./cmd/mr-index

# 2. Build the skill index (embeds every installed skill once)
./bin/mr-index build
# → built 209 skills (dim 768) → ~/.meta-router/index.json

# 3. Smoke-test the hook with a sample prompt
echo '{"prompt":"run the qa checks on this branch and write tests"}' | ./bin/mr-hook
# → a JSON line with hookSpecificOutput.additionalContext listing relevant skills,
#   OR no output if nothing clears the gate (that is correct, fail-open behaviour)
```

If that prints a context line (or cleanly prints nothing), you're ready to register it.

### Register the hook

meta-router runs as two Claude Code hooks. **It never edits `settings.json` for you** — registering it is your explicit action. Merge this into `~/.claude/settings.json` (use absolute paths to the binaries you just built):

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "/absolute/path/to/bin/mr-hook", "args": [] }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "/absolute/path/to/bin/mr-index", "args": ["refresh"] }
        ]
      }
    ]
  }
}
```

| Hook | Fires on | Effect |
|---|---|---|
| `UserPromptSubmit` | every prompt | `mr-hook` retrieves the top-k skills and injects them as `additionalContext`. Always exits 0 (fail-open). |
| `SessionStart` | each new / resumed / cleared session | `mr-index refresh` re-embeds only the skills whose files changed (hash-diff), keeping the index fresh and cheap. |

> On Windows, supplying `args` makes Claude Code spawn the binary directly without a shell, which avoids any misinterpretation of a `C:\\...` path. `mr-hook` takes no arguments (it reads the prompt JSON from stdin); `mr-index` takes `refresh` as a single argument.

To disable instantly, remove the two hook entries again — nothing else persists except the index and log under `~/.meta-router/`.

## Usage

### `mr-index` — build and refresh the index

```bash
mr-index build      # embed all skills from scratch → ~/.meta-router/index.json
mr-index refresh    # hash-diff: re-embed only changed/new skills, drop removed ones (fast)
```

Flags: `-skill-roots` (comma-separated, default `~/.claude/skills`), `-endpoint` (default `http://127.0.0.1:11436`), `-out` (default `~/.meta-router/index.json`).

The indexer walks each root for `*/SKILL.md`, parses the YAML frontmatter (`name`, `description`, `when_to_use` — including block-scalar `>`/`|` descriptions, which most skills use), dedups by id, and embeds the combined text. Unparseable skills are skipped, never fatal.

### `mr-hook` — the per-prompt surfacer

Reads `{"prompt": "..."}` on stdin, emits the hook JSON on stdout. You normally don't call it by hand, but it's fully testable:

```bash
echo '{"prompt":"debug this failing integration test"}' | mr-hook
```

Tuning flags (pass them in the hook `command`, e.g. `mr-hook -min-cosine 0.60`):

| Flag | Default | Purpose |
|---|---|---|
| `-min-cosine` | `0.55` | Confidence gate: minimum top cosine to surface anything. Raise it if irrelevant skills appear; lower it if relevant ones are missed. |
| `-k` | `3` | Max skills to surface per prompt. |
| `-min-len` | `12` | Min trimmed prompt length (chars) before retrieval is attempted. |
| `-timeout-ms` | `300` | Hard deadline for the whole retrieve. On overrun, surface nothing. |
| `-endpoint` | `http://127.0.0.1:11436` | Embedding endpoint. |
| `-index` | `~/.meta-router/index.json` | Index path. |
| `-log` | `~/.meta-router/usage.jsonl` | Usage-log path. |

### `mr-eval` — measure retrieval quality

A benchmarking tool that scores retrievers (BM25, embedding-only, hybrid) against a labeled gold-set, reporting recall@1/@3/@5, MRR, and median latency — useful for tuning or for validating a change to the retrieval logic.

```bash
mr-eval -goldset testdata/goldset.jsonl
```

## How it works

The pipeline is **retrieve → fuse → gate → inject**:

```
prompt (stdin JSON)
   │
   ├─ too short? ───────────────────────────────────► surface nothing
   │
   ▼
Load ~/.meta-router/index.json  (cached skill vectors, built once)
   │
   ├──────────────┐
   ▼              ▼
BM25 over      EmbeddingGemma cosine
skill texts    (embed the QUERY once,
(lexical)       score vs cached vectors)
   │              │  └─► top cosine = confidence signal
   └──────┬───────┘
          ▼
   Reciprocal Rank Fusion (RRF)  →  fused top-k
          │
          ▼
   top cosine ≥ -min-cosine ?  ──no──►  surface nothing (gated-empty)
          │ yes
          ▼
   inject top-k skills as additionalContext  →  stdout hook JSON
```

Key properties:

- **The index is built once; only the query is embedded per prompt.** Skill vectors are cached on disk (JSON, ~200 skills × 768 floats loads in well under the latency budget), so the hot path is a single small embedding call plus in-memory math.
- **RRF fuses the two rankings** rather than trusting either alone — lexical catches exact term matches, semantic catches paraphrase, and rank fusion is robust to the score scales differing.
- **The gate uses the top raw cosine** as a confidence floor: a prompt with no good semantic match surfaces nothing, which is what keeps the hook quiet and trustworthy.
- **Fail-open is absolute.** No index, cold embedder, malformed input, or blown deadline all resolve to "inject nothing, exit 0." When the embedder is unavailable the hook records the mode and stays silent; the BM25 path is wired for future gated use.
- **Hash-diff refresh** keeps the index current: each entry stores a hash of exactly the embedded text, so `refresh` re-embeds only what changed.
- **Privacy:** the usage log (`~/.meta-router/usage.jsonl`) records a SHA-256 hash of the prompt, its length, which skills were surfaced, the top cosine, latency, and the decision mode — never the raw prompt.

## Requirements

- **Go 1.26+** to build.
- A **local OpenAI-compatible embedding endpoint** serving an `embeddinggemma` model (POST `/v1/embeddings`), reachable at `http://127.0.0.1:11436` by default. No cloud account or API key is required. The hook reuses a warm local embedder; it ships no model of its own.
- **Claude Code** with hooks support (`UserPromptSubmit` injecting `additionalContext`, `SessionStart` running a command).

## What it does NOT do

Being honest about scope:

- **It does not route models or pick agents.** v1 is purely the *inward* axis — skill awareness. It does not choose between cloud models, orchestrate multi-agent runs, or do quota/budget accounting.
- **It cannot make Claude Code retrieve MCP tools on demand.** It surfaces *skills* (`SKILL.md` files) as context; it does not filter or page the MCP tool list. The offload feature is a one-line text nudge only — it does not call any tool for you.
- **It does not auto-edit your `settings.json`.** Registering and removing the hooks is always your explicit action.
- **It does not install, modify, or recommend installing skills.** It only ranks and surfaces what you already have.
- **It does not guarantee a suggestion every prompt.** By design it stays silent when nothing clears the confidence gate — empty output is correct, not a failure.
- **It depends on a local embedder for the semantic half.** If that endpoint is down, the hook fails open (surfaces nothing) rather than degrading silently to lexical-only.

## Roadmap

meta-router is one "capability router" framed around three axes — *given this task, what's the best capability?* — built local-first, one shippable layer at a time:

- **v1 — Skill awareness (shipped).** The per-prompt skill surfacer described above.
- **v2 — Local offload routing (shipped).** Detect mechanical text work and steer it to free local tools (the offload nudge is the first piece of this).
- **v3 — Headless multi-agent orchestration (planned).** A quota-aware orchestrator that routes across capabilities, picking the best single tool, combination, or sequence per task.

## Contributing

Contributions are welcome. Build with `go build ./...` and run the full suite with `go test ./...` before opening a PR. The retrieval logic lives in `internal/retrievers/` (BM25, embedding, RRF hybrid), the index in `internal/index/`, and the skill parser in `internal/catalog/`; `mr-eval` is the tool to validate any change to ranking quality.

## Security

meta-router runs entirely on your machine and sends prompt text only to the local embedding endpoint you configure — never to any third party. The usage log stores only hashed prompts. If you find a security issue, please report it privately rather than opening a public issue.

## License

Licensed under the [Apache License 2.0](LICENSE).
