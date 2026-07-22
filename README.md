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

**meta-router** is a `UserPromptSubmit` hook that fixes this with *retrieve-before-expose*: for every prompt, it retrieves the few most relevant installed skills using semantic search over a locally built index, and injects them as `additionalContext`. The model sees exactly the skills that matter for *this* task, even if you have hundreds installed. It runs entirely on your machine — no API keys, no data leaves the box — by reusing a local embedding endpoint you already have. And it is **strictly additive and fail-open**: on any error, timeout, or cold embedder, it degrades to a precision-gated lexical fallback or to silence, and always exits cleanly — it can never block or break a prompt.

## Features

- **Fixes the dropped-long-tail problem** — relevant skills get surfaced regardless of how many you have installed.
- **Covers plugin skills too** — indexes `~/.claude/skills` *and* every installed plugin's skills (superpowers, huggingface-skills, …), surfacing them under their invocable names (`superpowers:brainstorming`).
- **Embed-primary retrieval** — EmbeddingGemma cosine ranking (measured better than the previous BM25+RRF hybrid on the gold-set; the hybrid remains available via `-ranker=hybrid`).
- **Confidence-gated** — only surfaces when the top semantic match clears a cosine threshold, so quiet prompts stay quiet (no noise).
- **Fully local & private** — embeddings run against a local OpenAI-compatible endpoint; prompts are never sent to any cloud, and the usage log stores only a hash + length, never raw text.
- **Fail-open by design** — a hard per-prompt deadline, a ~200 ms connect timeout so a dead embedder fails fast, a *precision-gated* BM25 fallback when the embedder is down (it surfaces the single top lexical match only on overwhelming evidence — otherwise silence), and an unconditional clean exit. It cannot wedge your prompt.
- **Cheap incremental index** — a hash-diff `refresh` re-embeds only the skills whose content changed, fast enough to run on every session start; a `refresh.log` status line and a mass-removal guard (`--force` to override) make it safe to run unattended.
- **Fast per-prompt loads** — a binary `index.bin` sidecar (float32 vectors, gob) parses ~10× faster than the JSON index; JSON stays the source of truth and the automatic fallback.
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

To disable instantly, remove the two hook entries again — nothing else persists except the files under `~/.meta-router/` (`index.json` + `index.bin` sidecar, one dated `index.json.bak-*`, `roots.json`, `refresh.log`, `usage.jsonl`, and — once the outcome hook is wired — `outcomes.jsonl`).

## Usage

### `mr-index` — build and refresh the index

```bash
mr-index build      # embed all skills from scratch → ~/.meta-router/index.json (+ index.bin sidecar)
mr-index refresh    # hash-diff: re-embed only changed/new skills, drop removed ones (fast)
```

Flags: `-skill-roots` (comma-separated override), `-endpoint` (empty = per-machine resolution: `$MR_EMBED_ENDPOINT`, then `~/.meta-router/endpoints.json`, then the built-in `:11436`→`:18793` failover chain), `-out` (default `~/.meta-router/index.json`), `-force` (refresh only: allow removing >30% of entries).

**Root discovery & `roots.json`.** With no `-skill-roots`, the root set is `~/.claude/skills` (the user pack) plus every installed plugin's skills dir, discovered from `~/.claude/plugins/installed_plugins.json` (which pins each plugin's active version; a direct cache scan is the fallback). `build` re-discovers and persists the set to `roots.json` next to the index; `refresh` reads `roots.json` (creating it if absent) — so the no-flags SessionStart `mr-index refresh` always sees the full set without touching `settings.json`.

The indexer walks each root for `SKILL.md` files (skipping hidden dirs like `.agents/`, installer `temp_git_*`/`temp_subdir_*` clones, and `node_modules`), parses the YAML frontmatter (`name`, `description`, `when_to_use` — including block-scalar `>`/`|` descriptions), collapses description-identical twin copies to the top-level invocable one, dedups by id, and embeds the combined text. Skills are identified by their **invocable** name: the skill's dir name for user skills, `<plugin>:<skill>` for plugin skills. Unparseable skills are skipped, never fatal.

**Refresh safety.** Every `refresh` run appends one JSON status line to `refresh.log` (timestamp, entries before/after, added/removed/re-embedded, duration, ok/error). A refresh that would remove more than 30% of existing entries — usually a symptom of a wrong root set, not of mass uninstalls — is refused, printing exactly what it would remove; rerun with `-force` if intended. Each index overwrite also keeps exactly one dated backup (`index.json.bak-YYYYMMDD-HHMMSS`), pruning older ones.

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
| `-min-len` | `6` | Min trimmed prompt length (chars) before retrieval is attempted. |
| `-ranker` | `embed` | Primary ranking: `embed` (cosine-only) or `hybrid` (BM25+embed RRF). |
| `-timeout-ms` | `300` | Hard deadline for the whole retrieve. On overrun, surface nothing. |
| `-endpoint` | *(empty)* | Embedding endpoint. Empty = per-machine resolution: `$MR_EMBED_ENDPOINT`, then `~/.meta-router/endpoints.json`, then the `:11436`→`:18793` failover chain. Set it to pin one endpoint exactly. |
| `-index` | `~/.meta-router/index.json` | Index path (`index.bin` sidecar is used automatically when fresh). |
| `-log` | `~/.meta-router/usage.jsonl` | Usage-log path. |

### `mr-eval` — measure retrieval quality

A benchmarking tool that scores retrievers (BM25, embedding-only, hybrid) against a labeled gold-set, reporting recall@1/@3/@5, MRR, and median latency — useful for tuning or for validating a change to the retrieval logic. It evaluates over the same discovered root set the hook indexes, and reports both the full gold-set and the *covered-only* subset (cases whose expected skill is actually installed), so uninstalled targets can't mask ranking regressions.

```bash
mr-eval -goldset testdata/goldset.jsonl
```

### `mr-outcomes` — did surfaced skills get used?

Joins `usage.jsonl` surfacings with Skill-tool invocations and reports the surfaced→invoked hit-rate, overall and per skill:

```bash
mr-outcomes                 # ~/.meta-router/{usage,outcomes}.jsonl, 30-minute window
mr-outcomes -window-min 10  # stricter attribution
```

It reads `~/.meta-router/outcomes.jsonl`, one JSON object per line:

```json
{"ts_unix": 1751600000, "skill": "superpowers:brainstorming"}
```

where `skill` is the invocable skill name exactly as the Skill tool receives it (identical to the ids mr-hook surfaces). The file is expected to be written by a `PostToolUse` hook on the Skill tool — wiring that hook is a deployment step outside these binaries; until it exists, `mr-outcomes` reports the surfacing side against zero invocations.

## How it works

The pipeline is **retrieve → gate → inject**:

```
prompt (stdin JSON)
   │
   ├─ too short? ───────────────────────────────────► surface nothing
   │
   ▼
Load ~/.meta-router/index.bin (fast sidecar; falls back to index.json)
   │
   ▼
EmbeddingGemma cosine ranking          embedder down?
(embed the QUERY once,          ──────► BM25 fallback under a strict
 score vs cached vectors)               precision gate: surface the single
   │  └─► top cosine = confidence       top lexical match only on
   ▼      signal                        overwhelming evidence, else silence
top cosine ≥ -min-cosine ?  ──no──►  surface nothing (gated-empty)
   │ yes
   ▼
inject top-k skills as additionalContext  →  stdout hook JSON
```

Key properties:

- **The index is built once; only the query is embedded per prompt.** Skill vectors are cached on disk (the gob/float32 `index.bin` sidecar parses in ~3 ms; the JSON is the source of truth and automatic fallback), so the hot path is a single small embedding call plus in-memory math.
- **Embed-primary ranking** — measured better than the BM25+RRF hybrid on the 236-case gold-set (covered-only recall@3 0.829 vs 0.732); the hybrid remains one flag away (`-ranker=hybrid`) and in `mr-eval` for comparison.
- **The gate uses the top raw cosine** as a confidence floor: a prompt with no good semantic match surfaces nothing, which is what keeps the hook quiet and trustworthy.
- **Fail-open is absolute.** No index, malformed input, or blown deadline resolve to "inject nothing, exit 0." A cold/dead embedder fails the dial in ~200 ms and drops to the precision-gated BM25 fallback — tuned on the gold-set for zero wrong surfacings (a wrong fallback surfacing is worse than silence).
- **Hash-diff refresh** keeps the index current: each entry stores a hash of exactly the embedded text, so `refresh` re-embeds only what changed — with a status line per run in `refresh.log`, a >30% mass-removal guard, and a single dated `.bak` of the replaced index.
- **Privacy:** the usage log (`~/.meta-router/usage.jsonl`) records a SHA-256 hash of the prompt, its length, which skills were surfaced, the top cosine, latency, and the decision mode — never the raw prompt.

## Requirements

- **Go 1.26+** to build.
- A **local OpenAI-compatible embedding endpoint** serving an `embeddinggemma` model (POST `/v1/embeddings`), reachable at `http://127.0.0.1:11436` by default (override with `-endpoint`). No cloud account or API key is required. The hook reuses a warm local embedder; it ships no model of its own.
- **Claude Code** with hooks support (`UserPromptSubmit` injecting `additionalContext`, `SessionStart` running a command).

### Recipe: a dedicated `llama-server` sidecar

Any server exposing OpenAI-compatible `/v1/embeddings` works. If you don't already run one, a single [llama.cpp](https://github.com/ggml-org/llama.cpp) `llama-server` binary is the simplest sidecar — and on Windows it runs natively (`llama-server.exe`), which removes any WSL or Docker dependency:

```bash
# 1. Grab a llama.cpp release binary + an EmbeddingGemma GGUF, then serve it on a spare port:
llama-server --embeddings -m embeddinggemma-300M-Q8_0.gguf --host 127.0.0.1 --port 18793
# 2. Point the indexer at it:
mr-index build -endpoint http://127.0.0.1:18793
# 3. And add the same flag to the hook command in settings.json:
#    "command": "/absolute/path/to/bin/mr-hook", "args": ["-endpoint", "http://127.0.0.1:18793"]
```

The model stays resident in the sidecar, so per-prompt query embeddings are a few milliseconds — well inside `mr-hook`'s 300 ms deadline.

## What it does NOT do

Being honest about scope:

- **It does not route or *call* models/agents.** What ships today is the *inward* axis — it surfaces relevant skills and nudges toward free local offload tools, both as injected context text. It does not choose between cloud models, orchestrate multi-agent runs, or do quota/budget accounting (that is the planned v3).
- **It cannot make Claude Code retrieve MCP tools on demand.** It surfaces *skills* (`SKILL.md` files) as context; it does not filter or page the MCP tool list. The offload feature is a one-line text nudge only — it does not call any tool for you.
- **It does not auto-edit your `settings.json`.** Registering and removing the hooks is always your explicit action.
- **It does not install, modify, or recommend installing skills.** It only ranks and surfaces what you already have.
- **It does not guarantee a suggestion every prompt.** By design it stays silent when nothing clears the confidence gate — empty output is correct, not a failure.
- **It depends on a local embedder for the semantic ranking.** If that endpoint is down, the hook only surfaces the single top lexical match when the BM25 evidence is overwhelming (a gate tuned for precision on the gold-set) — otherwise it surfaces nothing rather than guessing.

## Roadmap

meta-router is one "capability router" framed around three axes — *given this task, what's the best capability?* — built local-first, one shippable layer at a time:

- **v1 — Skill awareness (shipped).** The per-prompt skill surfacer described above.
- **v2 — Offload nudge (shipped + live).** Detects mechanical text work (summarize / classify / extract / triage over a pasted block) and injects a one-line nudge toward free local offload tools. It is a *suggestion*, not routing — it never calls a tool; the actual local-offload execution lives in the companion [offload-harness](https://github.com/dmmdea/offload-harness) project.
- **v3 — Headless multi-agent orchestration (planned).** A quota-aware orchestrator that routes across capabilities, picking the best single tool, combination, or sequence per task.

## Contributing

Contributions are welcome. Build with `go build ./...` and run the full suite with `go test ./...` before opening a PR. The retrieval logic lives in `internal/retrievers/` (BM25, embedding, RRF hybrid), the index in `internal/index/`, and the skill parser in `internal/catalog/`; `mr-eval` is the tool to validate any change to ranking quality.

## Security

meta-router runs entirely on your machine and sends prompt text only to the local embedding endpoint you configure — never to any third party. The usage log stores only hashed prompts. If you find a security issue, please report it privately rather than opening a public issue.

## The v3 orchestrator + eval harness (v0.5.0)

Beyond the surfacer, this repo now carries the **multi-lane orchestrator** (`mr-orchestrate`): it routes headless tasks across claude / codex / GLM / local-model lanes under a quota ledger with admission gates, burn-rate pacing (E1 downshift when a window is on pace to blow, E2 spend-down boost steering batch-tagged work into a window about to strand unused budget), and per-lane error handling. State lives under `~/.meta-router/orchestrate/` (config, ledger, dispatch receipts) — nothing operator-specific is in the repo.

The **eval substrate** ships too: a routing gold-set schema + verifier engine (`internal/goldtask`), an execution verifier harness (`mr-goldverify`: checkout parent → apply candidate diff → run held-out tests), and a local-verifier ceiling meter (`mr-verifier`, AURC/AUGRC). **Bring your own gold set**: point `-goldset` at your own task JSONL; the repo's gold-set-dependent tests skip when none is present.

## License

Licensed under the [Apache License 2.0](LICENSE).
