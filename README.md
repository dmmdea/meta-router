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

meta-router runs as two Claude Code hooks. Register them **by hand** (below), or let `mr-orchestrate install claude` do it — see [Installing into a host](#installing-into-a-host). Nothing is ever wired without you asking: the hook binaries themselves never touch `settings.json`.

Merge this into `~/.claude/settings.json` (use absolute paths to the binaries you just built):

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

Flags: `-skill-roots` (comma-separated override), `-endpoint` (empty = per-machine resolution: `$MR_EMBED_ENDPOINT`, then `~/.meta-router/endpoints.json`, then the built-in `:11436`→`:18793` failover chain), `-out` (default `~/.meta-router/index.json`), `-force` (refresh only: allow removing >30% of entries), `-embed-model` / `-tpl` (build only — see below).

**Per-model embedding templates (W9-P).** Embedding models are prompt-trained: each expects its own wrapper around queries and documents (EmbeddingGemma's card specifies `task: search result | query: {q}` / `title: none | text: {d}`; Qwen3-Embedding wants an instruct-prefixed query, plain documents, and an explicit EOS under last-token pooling). `internal/embedtpl` is the versioned registry of those templates. Templating is **opt-in per build**: `mr-index build -tpl tpl1` embeds every document through the registry template and records the version in the index identity (`"model": "embeddinggemma/tpl1"`); the default remains untemplated raw text, byte-identical to prior releases. `refresh` always preserves the index's recorded identity — changing model or template is a rebuild (`-embed-model` picks the model, e.g. `qwen3-embedding-4b-q4`), and passing either flag to `refresh` is rejected **by flag presence**, even at its default value. At query time `mr-hook` resolves the identity and templates the query the same way; an identity naming a template (or version) the binary does not know is **refused fail-open** — the hook surfaces nothing but the quota hint, logs `mode:"tpl-mismatch"`, and `refresh` refuses before touching the index.

A templated index also carries a `tpl_guard` field — a tripwire against **stale `mr-index` binaries**: a pre-template binary's refresh re-embeds every skill raw (its hashes all miss) while preserving the identity verbatim, and the only detectable trace is that its save drops the unknown `tpl_guard` field. A templated identity whose guard is missing or mismatched is therefore refused with a `template guard` error naming the rebuild (`mr-index build -tpl …`). The hook-side hazard has no in-file fence: an **older `mr-hook`** knows nothing about identities and would query templated vectors raw. So deploy new binaries fleet-wide **before** building a templated index — that ordering is the whole protection for mixed fleets.

**Root discovery & `roots.json`.** With no `-skill-roots`, the root set is `~/.claude/skills` (the user pack) plus every installed plugin's skills dir, discovered from `~/.claude/plugins/installed_plugins.json` (which pins each plugin's active version; a direct cache scan is the fallback). `build` re-discovers and persists the set to `roots.json` next to the index; `refresh` reads `roots.json` (creating it if absent) — so the no-flags SessionStart `mr-index refresh` always sees the full set without touching `settings.json`.

The indexer walks each root for `SKILL.md` files (skipping hidden dirs like `.agents/`, installer `temp_git_*`/`temp_subdir_*` clones, and `node_modules`), parses the YAML frontmatter (`name`, `description`, `when_to_use` — including block-scalar `>`/`|` descriptions), collapses description-identical twin copies to the top-level invocable one, dedups by id, and embeds the combined text. Skills are identified by their **invocable** name: the skill's dir name for user skills, `<plugin>:<skill>` for plugin skills. Unparseable skills are skipped, never fatal.

**Refresh safety.** Every `refresh` run appends one JSON status line to `refresh.log` (timestamp, entries before/after, added/removed/re-embedded, duration, ok/error, and the index `identity` the run operated on — so a templated deployment silently reverting to raw after an index loss leaves a durable trace). A refresh that would remove more than 30% of existing entries — usually a symptom of a wrong root set, not of mass uninstalls — is refused, printing exactly what it would remove; rerun with `-force` if intended. Each index overwrite also keeps exactly one dated backup (`index.json.bak-YYYYMMDD-HHMMSS`), pruning older ones.

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
| `-ranker` | `embed` | Primary ranking: `embed` (cosine-only), `rerank` (embed, then a `bge-reranker-v2-m3` reorder — better recall on real prompts, but see the deadline note below), or `hybrid` (BM25+embed RRF). An unrecognized value runs `embed` and records why in `err`. |
| `-timeout-ms` | `300` | Hard deadline for the whole retrieve. On overrun, surface nothing. **`-ranker=rerank` requires ≥ 6000 and works best at 8000** — the cross-encoder is CPU-bound and costs ~2.1 s p50 / ~4.5 s worst case. Below 6000 the ranker refuses, logs why, and serves `embed`. |
| `-endpoint` | *(empty)* | Embedding endpoint. Empty = per-machine resolution: `$MR_EMBED_ENDPOINT`, then `~/.meta-router/endpoints.json`, then the `:11436`→`:18793` failover chain. Set it to pin one endpoint exactly. |
| `-index` | `~/.meta-router/index.json` | Index path (`index.bin` sidecar is used automatically when fresh). The index's recorded identity (`model`, optionally `model/tplN`) decides how the query is embedded; an unknown template version is refused fail-open (`mode:"tpl-mismatch"`, quota hint only). |
| `-log` | `~/.meta-router/usage.jsonl` | Usage-log path. |

### Installing into a host

Wiring meta-router by hand is fine for one machine and tedious for a fleet, so
`mr-orchestrate` can do it — under a rule that makes it safe to run twice, and
safe to undo:

```bash
mr-orchestrate install claude -dry-run   # print the exact plan, write nothing
mr-orchestrate install claude            # wire it
mr-orchestrate uninstall claude          # put everything back
```

| Host | What gets wired |
|---|---|
| `claude` | `UserPromptSubmit` → `mr-hook`, `SessionStart` → `mr-index refresh`, `statusLine` → the quota tee (wrapping whatever statusline you already had), and the `meta-router` MCP server |
| `codex` | the `meta-router` MCP server (Codex has no hook or statusline surface) |

**Ownership is recorded, never inferred.** Every change is written to a manifest
at `<home>/.meta-router/orchestrate/install/<host>.json`, and that manifest is
the *only* authority for what `uninstall` may remove. An entry that merely looks
like meta-router's but is not in the manifest belongs to whoever wrote it:
`install` refuses to adopt it and `uninstall` refuses to delete it. A run is
all-or-nothing — one conflict refuses the whole thing, and a failure part-way
rolls back what it had already written.

**Uninstall tells you which kind of restore it did.** If a managed file still
holds exactly what `install` left, the pre-install copy is restored *byte for
byte* — your formatting and key order included. If you have edited that file
since, your edits win: only the recorded entries are removed, and the result is
reported as `restore: surgical` rather than claiming a byte restore it did not
perform.

Both MCP registries (`~/.claude.json`, `~/.codex/config.toml`) are written by
each host's **own** CLI (`claude mcp add` / `codex mcp add`), never by us: those
files are live state the host rewrites while it runs, so a read-modify-write
from a second process would silently drop whatever it wrote in between.

Useful flags: `-home <dir>` wires a different home and isolates the *entire*
install under it (config, manifest, backups, tee), which is how the test suite
exercises this without touching a real machine; `-bin <dir>` points at the
deployed binaries (default `<home>/.meta-router/bin`); `-json` emits the report
as JSON.

### `mr-orchestrate report` — the receipts dashboard

Every dispatch appends a receipt to `dispatch.jsonl`; `report` renders that log
into an operator dashboard — per-lane runs/tokens/spend, outcome classes,
silent-fallback pairs (requested→actual model, the signal `attributed_models`
exists to carry), rotation reasons, quality verdicts, the S2R-10 adherence
block, and per-day activity:

```bash
mr-orchestrate report              # whole log, human dashboard
mr-orchestrate report -days 7     # last 7 days — anchored to the newest receipt, never the clock
mr-orchestrate report -lane claude -json
mr-orchestrate report -lane "(none)"   # receipts that carry no lane
```

It is offline by construction (no network, no ledger transaction, no clock), so
the same log renders identically on every run — the `-days` window is
`[newest receipt − N·24h, newest receipt]`, cutoff instant included. It is also
honest about the log itself, on the principle that a dashboard's *clean* states
must be falsifiable:

- unparseable lines are counted and shown as a `WARN`;
- receipts that parse but carry **no timestamp** get their own surfaced counter —
  they cannot be windowed, so a `-days` view excludes them *and says so* (the
  whole-log view counts them but keeps them out of span/activity);
- the fallback line always reports its own detector coverage
  (`attribution present on N/M executed`) — "0 fallbacks" over receipts that
  carry no attribution is a broken capture pipeline, not health;
- an absent log is "no receipts yet" (`"log_absent": true` under `-json`);
  an unreadable or torn-read log is an **error** — never an empty-but-healthy
  dashboard;
- empty `model`/`outcome_class`/lane fields bucket as `(none)` so the aggregate
  lines reconcile with the header totals, and zero-denominator percentages
  render `n/a`, not `0.0%`.

### Resilience (W6)

Four guards keep a bad hour from becoming a bad day, each with a canary test
that goes red if the guard is reverted:

- **Self-healing lane exclusion** — a lane whose *adapter* keeps failing
  (`spawn_error`/`parse_error`; `api_error` on local) is masked with a
  progressive backoff (1m → 2m → … capped 30m) and re-admitted when it
  expires; one healthy dispatch clears it. Quota signals never open this
  breaker — admission already prices those. Kill-switch: `exclusion_off`.
- **Incident mode** (`incident_mode_on`, ships OFF) — when most lanes are
  pressured, the router stops demoting throttled lanes (+1 shadow prices
  suspended) and routes by pure rank among survivors: with no calmer lane to
  shed onto, the demotion only picks a worse model. Off by default pending
  eval evidence, same posture as `pace_rank_on`.
- **Typed 429** — receipts carry `rate_limit_origin`: `upstream` (the vendor
  said no; ledger-eligible) vs `local` (our own limiter; never recorded as
  vendor exhaustion).
- **Local sliding-window limiter** (`local_max_per_min`, default 20/60s;
  negative disables) — headless bursts can't wedge the local box the operator
  is also using interactively. A denial relegates (exit 3) with a typed
  receipt so the DAG escalates to a cloud lane.

### Lossless compaction + context handoff (W5)

Strategy DAGs move dep results between nodes; W5 makes those bytes cheaper
without ever risking their meaning (DG-3: *quality is the lever* — anything
lossy belongs behind a fidelity gate and does not exist here):

- **Embed-time compaction** — a dep artifact that is JSON is fenced into the
  downstream prompt minified and column-rotated (`internal/orch/compact`,
  `Decompact(Compact(x))` semantically equal to `x`, marker collisions
  escaped bijectively). The stored artifact keeps its original bytes; savings
  are journaled per node (`ctx_compacted`) as a side-effect metric. Prose
  embeds byte-identical. Kill-switch: `compaction_off`.
- **Context handoff on re-lane** — a re-laned retry carries the failed
  attempt's lane, outcome class, and a bounded excerpt of its result in a
  `<handoff prior-attempt>` block, so the alternative lane starts from state
  instead of cold.

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
- **Privacy:** the usage log (`~/.meta-router/usage.jsonl`) records a SHA-256 hash of the prompt, its length, the `session_id`/`prompt_id` Claude Code supplies on the hook payload, which skills were surfaced, the top cosine, latency, and the decision mode — never the raw prompt. Note that `session_id` is the filename of the session transcript, which does contain prompt text: see [SECURITY.md](SECURITY.md) before sharing the log.

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
- **It does not edit your `settings.json` behind your back.** The hook binaries never write to it. `mr-orchestrate install` does — that is its whole job — but only when you run it, only to entries it records in a manifest, and never over anything it did not write. Run it with `-dry-run` first; it prints the exact plan and writes nothing.
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
