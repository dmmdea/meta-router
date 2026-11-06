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
- **Confidence-gated** — only surfaces when the top semantic match clears a cosine threshold, and every skill it shows clears that threshold itself and is one the model can invoke, so quiet prompts stay quiet (no noise).
- **Fully local & private** — embeddings run against a local OpenAI-compatible endpoint; prompts are never sent to any cloud, and the usage log stores only a hash + length, never raw text.
- **Fail-open by design** — a hard per-prompt deadline, a ~200 ms connect timeout so a dead embedder fails fast, silence when the embedder is down or not loaded (there is no lexical fallback: measured in production, its 206 hints were followed 3 times), and an unconditional clean exit. An unknown flag is recorded and ignored rather than failing the hook. It cannot wedge your prompt.
- **Cheap incremental index** — a hash-diff `refresh` re-embeds only the skills whose content changed, fast enough to run on every session start; a refresh that changes nothing writes nothing, overlapping refreshes take a lock instead of racing, and a `refresh.log` status line and a mass-removal guard (`--force` to override) make it safe to run unattended.
- **Fast per-prompt loads** — a binary `index.bin` sidecar (float32 vectors, gob) parses ~10× faster than the JSON index; JSON stays the source of truth and the automatic fallback, and if the JSON itself is torn the load falls back to the sidecar and then to the dated backup.
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
| `UserPromptSubmit` | every prompt | `mr-hook` retrieves the top-k skills and injects them as `additionalContext`. Background-task notifications get no skill hint. Always exits 0 (fail-open). |
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

**Flat roots and ID namespaces.** A `roots.json` entry may carry a `"kind"`. The zero value (or `"skills"`) is the `SKILL.md` directory walk above. Three FLAT kinds index a directory of bare `*.md` files (frontmatter `description` required; empty descriptions are skipped rather than embedded as name-only noise), each minting its own invocable identity: `"commands"` → `/<basename>` (slash commands), `"agents"` → `agent:<frontmatter name>` (dispatched via the Agent tool; the hook surfaces the bare name labeled `(agent)`), and `"tools"` → `tool:<name>` — POINTER entries for capabilities that are none of the above, e.g. MCP tools a task-shaped prompt should route to; the entry's description names the actual tool and route, and the hook surfaces it as `<name> (local tool)` with a trailer telling the model to load it via ToolSearch rather than the Skill tool. The `agent` and `tool` pack names are reserved for those namespaces and `roots.json` refuses a skills-class root that uses them (it would mint namespaced IDs for things that do not exist). Adding a kind is a capability: production composition changes only when a `roots.json` entry names such a root, and a typo'd kind is refused loudly with the valid set listed. Skills-class roots are always harvested before flat roots so a skill wins any description-twin collapse.

The indexer takes the skills the Skill tool can invoke by name: `<root>/SKILL.md` and `<root>/<name>/SKILL.md`, one level deep, following a top-level symlink or junction to a directory. It skips hidden dirs like `.agents/`, installer `temp_git_*`/`temp_subdir_*` clones, `node_modules`, and the user root's account-synced folder `synced/`; deeper `SKILL.md` files (pack-internal copies, test fixtures) are not invocable as named and are not indexed. It parses the YAML frontmatter (`name`, `description`, `when_to_use` — including block-scalar `>`/`|` descriptions), collapses description-identical twin copies to the top-level invocable one, dedups by id, and embeds the combined text. Skills are identified by their **invocable** name: the skill's dir name for user skills, `<plugin>:<skill>` for plugin skills. Unparseable skills are skipped, never fatal.

**Refresh safety.** Every `refresh` run appends one JSON status line to `refresh.log` (timestamp, entries before/after, added/removed/re-embedded, duration, ok/error, and the index `identity` the run operated on — so a templated deployment silently reverting to raw after an index loss leaves a durable trace). A refresh that would remove more than 30% of existing entries — usually a symptom of a wrong root set, not of mass uninstalls — is refused, printing exactly what it would remove; rerun with `-force` if intended. Each index overwrite also keeps exactly one dated backup (`index.json.bak-YYYYMMDD-HHMMSS`), pruning older ones. Runs do not race: a refresh takes `index.json.lock`, and one that finds it held logs `"skipped":"locked"` and exits 0 (a lock older than 10 minutes is a killed run's and is taken over). A refresh that changes nothing logs `"noop":true` and writes neither the index nor a backup. If `index.json` does not parse, the refresh loads the sidecar or the newest backup that does, rewrites the JSON and logs `"repaired_from"`.

### `mr-hook` — the per-prompt surfacer

Reads `{"prompt": "..."}` on stdin, emits the hook JSON on stdout. You normally don't call it by hand, but it's fully testable:

```bash
echo '{"prompt":"debug this failing integration test"}' | mr-hook
```

Tuning flags (pass them in the hook `command`, e.g. `mr-hook -min-cosine 0.60`):

| Flag | Default | Purpose |
|---|---|---|
| `-min-cosine` | `0.55` | Confidence gate: the top cosine must reach it for anything to surface, and each skill shown must reach it itself. Raise it if irrelevant skills appear; lower it if relevant ones are missed. |
| `-k` | `3` | Max skills to surface per prompt. |
| `-min-len` | `6` | Min trimmed prompt length (chars) before retrieval is attempted. |
| `-max-prompt-len` | `0` (off) | Prompts longer than this many bytes are logged `too-long` and not embedded. A long prompt is usually pasted material or a full brief, and a three-line skill hint under it is rarely the thing acted on: measured over 30 days, hints on prompts of 600 bytes or more were followed 1 time in 381, against 13 in 278 below that. Also settable per machine in `hook.json` (below). |
| `-holdout-pct` | `0` (off) | Percent (0..100) of **sessions**, chosen by a hash of the session id, that get everything computed and logged but are never shown the skill list. Rows carry `arm` (`treat` or `holdout`), and a holdout row lists what it withheld in `would_surface`, so `mr-outcomes` can compare how often a listed skill is invoked with and without the hint. Also settable per machine in `hook.json`. |
| `-ranker` | `embed` | Primary ranking: `embed` (cosine-only), `rerank` (embed, then a `bge-reranker-v2-m3` reorder — better recall on real prompts, but see the deadline note below), or `hybrid` (BM25+embed RRF). An unrecognized value runs `embed` and records why in `err`. |
| `-timeout-ms` | `300` | Hard deadline for the whole retrieve. On overrun, surface nothing. **`-ranker=rerank` requires ≥ 6000 and works best at 8000** — the cross-encoder is CPU-bound and costs ~2.1 s p50 / ~4.5 s worst case. Below 6000 the ranker refuses, logs why, and serves `embed`. |
| `-cold-skip` | `true` | Before embedding, ask the model router (llama-swap `GET /running`, 100 ms ceiling) whether the embed model is loaded. When it is not, or the router cannot be read, the hook surfaces nothing at once and sends one warm-up request, held open only until the router lists the model as loading (a few milliseconds; 150 ms ceiling) and then abandoned, instead of waiting out the embed budget for a model that needs seconds to load. Servers without a `/running` route embed exactly as before. Rows log `cold-skip-empty`; a run of cold skips that outlasts any real load (three earlier ones, the oldest two minutes old) logs `cold-stuck-empty` so an embedder that never loads is visible. **To turn it off pass `-cold-skip=false`, with the `=`:** Go's flag package reads `-cold-skip false` as `true` plus a stray argument. |
| `-endpoint` | *(empty)* | Embedding endpoint. Empty = per-machine resolution: `$MR_EMBED_ENDPOINT`, then `~/.meta-router/endpoints.json`, then the `:11436`→`:18793` failover chain. Set it to pin one endpoint exactly. |
| `-index` | `~/.meta-router/index.json` | Index path (`index.bin` sidecar is used automatically when fresh). The index's recorded identity (`model`, optionally `model/tplN`) decides how the query is embedded; an unknown template version is refused fail-open (`mode:"tpl-mismatch"`, quota hint only). |
| `-log` | `~/.meta-router/usage.jsonl` | Usage-log path. |

**Per-machine settings: `hook.json`.** `-max-prompt-len` and `-holdout-pct` can also be set in a file named `hook.json` beside the usage log (`~/.meta-router/hook.json` by default), for example `{"max_prompt_len": 600, "holdout_pct": 50}`. A flag given on the command line wins over the file. The file exists so these two knobs do not have to travel in a `settings.json` that several machines share: a hook binary older than 0.43.0 exits 2 on a flag it does not know, and exit 2 on `UserPromptSubmit` blocks the prompt. From 0.43.0 the hook itself parses flags fail-open: an unknown flag is recorded in the row's `err`, the flags after it are ignored, and the hook still exits 0.

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
| `claude` | `UserPromptSubmit` → `mr-hook`, `SessionStart` → `mr-index refresh`, `statusLine` → the quota tee (wrapping whatever statusline you already had), and the `mr-orchestrate` MCP server |
| `codex` | the `mr-orchestrate` MCP server (Codex has no hook or statusline surface) |

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

Joins `usage.jsonl` with Skill-tool invocations and reports how often a prompt that had a skill list was followed by a call of one of those skills, per holdout arm and per skill:

```bash
mr-outcomes            # ~/.meta-router/{usage,outcomes}.jsonl, last 30 days
mr-outcomes -days 7    # a shorter window; -days 0 reads every row
```

```
| arm     | prompts | followed | rate   |
| treat   |     ... |      ... |   ...% |   list shown
| holdout |     ... |      ... |   ...% |   list computed, withheld
treat minus holdout: ... points
skill calls: N, hinted: M, never hinted: ...%
```

**What the numbers mean.** A followed hint is agreement, not effect: the model may have invoked that skill anyway. The holdout row is the base rate, and the difference between the arms is what the hint adds. Run the hook with `-holdout-pct` (or `holdout_pct` in `hook.json`) to have a holdout arm; rows written without one are printed on a separate `no arm` line and are never mixed into the comparison.

The join is exact. An outcome is attributed to the prompt with the same `prompt_id`, or, without one, to the latest operator prompt of the same session before it; background-task notifications never count as that prompt. It reads `~/.meta-router/outcomes.jsonl`, one JSON object per line:

```json
{"ts_unix": 1751600000, "skill": "superpowers:brainstorming", "session_id": "...", "prompt_id": "...", "tool_use_id": "..."}
```

where `skill` is the invocable skill name exactly as the Skill tool receives it (identical to the ids `mr-hook` surfaces) and the three ids are the ones Claude Code puts on the `PostToolUse` payload. A row without a `session_id` cannot be attributed and is counted as skipped. The file is written by a `PostToolUse` hook on the Skill tool that you wire yourself; until it exists, `mr-outcomes` reports the surfacing side against zero invocations.

`-window-min N` additionally prints the older report, which credits any invocation within N minutes of a surfacing in any session. It reads high whenever sessions run concurrently and is kept only for comparison with old figures.

## How it works

The pipeline is **retrieve → gate → inject**:

```
prompt (stdin JSON)
   │
   ├─ too short, or longer than -max-prompt-len? ────► surface nothing
   │
   ▼
Load ~/.meta-router/index.bin (fast sidecar; falls back to index.json)
   │
   ▼
EmbeddingGemma cosine ranking          embedder down or not loaded?
(embed the QUERY once,          ──────► surface nothing (embedder-down,
 score vs cached vectors)               cold-skip-empty); a cold one gets
   │  └─► top cosine = confidence       one warm-up request for next time
   ▼      signal
top cosine ≥ -min-cosine ?  ──no──►  surface nothing (gated-empty)
   │ yes
   ▼
keep the candidates the model can invoke
whose own cosine ≥ -min-cosine  ──none──►  surface nothing (gated-empty)
   │
   ▼
inject up to k skills as additionalContext  →  stdout hook JSON
```

Key properties:

- **The index is built once; only the query is embedded per prompt.** Skill vectors are cached on disk (the gob/float32 `index.bin` sidecar parses in ~3 ms; the JSON is the source of truth and automatic fallback), so the hot path is a single small embedding call plus in-memory math.
- **Embed-primary ranking** — measured better than the BM25+RRF hybrid on the 236-case gold-set (covered-only recall@3 0.829 vs 0.732); the hybrid remains one flag away (`-ranker=hybrid`) and in `mr-eval` for comparison.
- **The gate uses the top raw cosine** as a confidence floor: a prompt with no good semantic match surfaces nothing, which is what keeps the hook quiet and trustworthy. Eligibility is decided before the cut: a skill the model cannot invoke (hidden by `skillOverrides`, from a disabled plugin, or with `disable-model-invocation`) takes no slot and cannot carry the gate for the others, and each skill shown clears the floor itself.
- **Machine turns get no skill hint.** Claude Code delivers background-task notifications (a finished background command or subagent, a Monitor event, including ones queued while the model was busy) through the same `UserPromptSubmit` event, as a prompt starting with `<task-notification>`. They are not requests: no retrieval runs, nothing reaches the embedder, and the row logs `mode:"machine-turn"`. The quota banner is decided exactly as on any other prompt, so a GLM hard-stop latch still shows on these turns.
- **Lane children get no skill hint either.** Every Claude process the orchestrator or a replay starts carries `MR_ORCH_DISPATCH=1`. No retrieval runs, the row logs `mode:"lane-child"`, and it gets no holdout arm, so a replay's hundreds of sessions neither keep the embedder loaded nor enter the experiment.
- **Fail-open is absolute.** No index, malformed input, an unknown flag, or a blown deadline all resolve to "inject nothing, exit 0." A dead embedder fails the dial in ~200 ms, and a cold one (a model router that has unloaded the model) is detected before the embed call and skipped at once. Either way the hook stays silent: an earlier lexical fallback was removed because its hints were almost never followed (206 shown, 3 followed, measured in production).
- **Hash-diff refresh** keeps the index current: each entry stores a hash of exactly the embedded text, so `refresh` re-embeds only what changed — with a status line per run in `refresh.log`, a >30% mass-removal guard, and a single dated `.bak` of the replaced index.
- **Privacy:** the usage log (`~/.meta-router/usage.jsonl`) records a SHA-256 hash of the prompt, its length, the `session_id`/`prompt_id` Claude Code supplies on the hook payload, which skills were surfaced, which ranked skills were withheld because the model cannot invoke them (`hidden`: skill IDs, never prompt text), the holdout arm and what a holdout row withheld (`arm`, `would_surface`: skill IDs again), the top cosine, latency, and the decision mode — never the raw prompt. Note that `session_id` is the filename of the session transcript, which does contain prompt text: see [SECURITY.md](SECURITY.md) before sharing the log.

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
- **It depends on a local embedder for the semantic ranking.** If that endpoint is down or the model is not loaded, the hook surfaces nothing rather than guessing.

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

Beyond the surfacer, this repo now carries the **multi-lane orchestrator** (`mr-orchestrate`): it routes headless tasks across claude / codex / copilot / GLM / local-model lanes — plus five FREE-FOREVER provider lanes (groq, cloudflare, openrouter `:free`, nim, gemini; credential files under `~/.meta-router/orchestrate/free/`, gemini allowlist-gated) — under a quota ledger with admission gates, burn-rate pacing (E1 downshift when a window is on pace to blow, E2 spend-down boost steering batch-tagged work into a window about to strand unused budget), and per-lane error handling. State lives under `~/.meta-router/orchestrate/` (config, ledger, dispatch receipts) — nothing operator-specific is in the repo.

The **eval substrate** ships too: a routing gold-set schema + verifier engine (`internal/goldtask`), an execution verifier harness (`mr-goldverify`: checkout parent → apply candidate diff → run held-out tests), and a local-verifier ceiling meter (`mr-verifier`, AURC/AUGRC). **Bring your own gold set**: point `-goldset` at your own task JSONL; the repo's gold-set-dependent tests skip when none is present.

`mr-verifier` can also measure an **external reference column** beside the local one, so a decisive accuracy measured on a small corpus is read against a second verifier rather than against nothing. Arming the reference column runs the same snippets, with the same question, past TypeSafe's Jev — a decision model that returns typed answers with probabilities instead of text — and prints the two ceilings side by side (decisive accuracy, coverage, effective ceiling, AURC, AUGRC). It is **off by default and it spends**: **$0.000701 for the committed 40-snippet corpus** on the live run of 2026-09-18, read out of the responses rather than estimated, at 223–675 ms per snippet (mean 309). The credential comes from `~/.meta-router/orchestrate/free/openrouter.token`, never from the environment; a missing file prints a warning and skips the column, and the run still exits 0 with whatever it could measure. The route and the served model are fixed constants in `internal/jevclient` — there is no flag, option or environment variable through which another model could be reached.

## License

Licensed under the [Apache License 2.0](LICENSE).

## Codespaces

The repo carries a [dev container](.devcontainer/devcontainer.json): the Go image, the `gh`
CLI, Node (for `@github/copilot`) on the smallest 2-core machine. Open it with **Code → Create
codespace on main**, then `go test ./...`. It exists so gold-probe verification (`go vet`, the
suites, `mr-goldverify`) can run off the operator's machines inside the plan's included
core-hours. Prebuilds are deliberately **not** enabled: they consume Actions minutes.
