# Design spec: v1.5 "tool-retrieval" — MCP tool surfacing

Status: **NO-GO (researched, not built)**. Kept as a design doc so the decision isn't re-litigated without new evidence.

## 1. The hypothesis

v1 (shipped) fixed skill-surfacing: Claude Code's built-in skill list is budgeted to a small
context slice with a per-skill description cap, and past ~30-50 installed skills the
least-recently-used ones get silently dropped. `mr-hook` compensates with retrieve-before-expose —
semantic search over a local index, injected as `additionalContext` on every `UserPromptSubmit`.

The hypothesis for v1.5: does the *same* problem exist for **MCP tools**, and can the *same*
pattern (a hook-based retriever indexing tool metadata, surfacing the top-k per prompt) fix it?

The README already flagged this as an explicit non-goal ("It cannot make Claude Code retrieve
MCP tools on demand... does not filter or page the MCP tool list") without previously researching
*why*. This doc is that research, plus the resulting go/no-go gate.

## 2. Findings

Each finding below was verified directly (WebFetch against the primary docs, or direct inspection
of this machine's live Claude Code state) — not relayed from a subagent's report unchecked.

### 2.1 Claude Code already ships the exact mechanism v1.5 would build

Confirmed **empirically, in this very session**: a system-reminder listed ~120 deferred MCP tool
names (from `gimp-mcp`, `local-offload`, `mem0`, `windows-mcp`) by name only, with an explicit
instruction: *"If the user's request might be served by one of these servers... call ToolSearch
with a relevant keyword."* Calling `ToolSearch` with `"select:TodoWrite"` returned the tool's full
JSON-schema definition inline, on demand.

This is Claude Code CLI's own built-in retrieve-before-expose implementation for tools — arguably
richer than `mr-hook`'s cosine-similarity ranking, since it supports exact-name (`select:`),
keyword, and required-term (`+term`) query forms. It runs at the harness/session level, not
through a hook, and needs no external index.

Cross-checked against the platform docs
([tool-search-tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool)):
this is a general Anthropic Messages-API capability (`tool_search_tool_regex_20251119` /
`tool_search_tool_bm25_20251119`, `defer_loading: true` per tool), available on Fable 5 / Opus 4.5+ /
Sonnet 4.5+ / Haiku 4.5. Claude Code enables it automatically once tool definitions would exceed
~10% of the context window — exactly the failure mode `mr-hook` v1 solves for skills, already
solved natively for tools.

**Consequence: the problem v1.5 was hypothesized to fix does not exist. Anthropic already fixed it,
above the hook layer, more richly than a hook could.**

### 2.2 Hooks have zero visibility into the tool/MCP surface

Verified directly against
[code.claude.com/docs/en/hooks.md](https://code.claude.com/docs/en/hooks.md). Full hook-event list:
`SessionStart`, `Setup`, `UserPromptSubmit`, `UserPromptExpansion`, `PreToolUse`,
`PermissionRequest`, `PermissionDenied`, `PostToolUse`, `PostToolUseFailure`, `PostToolBatch`,
`Notification`, `MessageDisplay`, `SubagentStart`, `SubagentStop`, `TaskCreated`, `TaskCompleted`,
`Stop`, `StopFailure`, `TeammateIdle`, `InstructionsLoaded`, `ConfigChange`, `CwdChanged`,
`FileChanged`, `WorktreeCreate`, `WorktreeRemove`, `PreCompact`, `PostCompact`, `Elicitation`,
`ElicitationResult`, `SessionEnd`.

**None of them carry a tool list, an MCP server list, or tool schemas in their input JSON.**
`SessionStart`'s documented input is `session_id`, `transcript_path`, `cwd`, `hook_event_name`,
`source`, `model`, `agent_type`, `session_title` — no tools field. `PreToolUse`/`PostToolUse` only
carry the single `tool_name`/`tool_input` of the tool actually being invoked, not the catalog of
what's available. A hook (mr-hook's only integration point into Claude Code) is structurally
blind to "what MCP tools are connected right now" — it would have to reimplement MCP client
discovery itself (spawn each configured server from `~/.claude.json`'s `mcpServers` entries, speak
`tools/list`) to even build a corpus, duplicating connections the harness already holds open.

### 2.3 No static, on-disk corpus of tool schemas exists to index

Verified directly by inspecting this machine's `~/.claude.json`: `mcpServers` entries store only
`type` / `command` / `args` (how to *launch* a server) — no cached tool list, no schemas.
`~/.claude/mcp-needs-auth-cache.json` is an auth-timestamp cache, unrelated. Tool schemas are
fetched over the MCP `tools/list` RPC at session start and live only in the running process's
memory; Claude Code exposes a curated, name-only *view* of them into context (the deferred-tools
system reminder) but never persists the full schemas to a file mr-index could embed ahead of a
prompt arriving.

This matters because `mr-index build` for skills works precisely *because* `SKILL.md` files are a
static corpus on disk, independent of any running session. No equivalent exists for MCP tools.

### 2.4 `additionalContext` cannot touch the `tools` array anyway

Even granting a hook that *could* discover tool schemas, the only lever a hook has is
`hookSpecificOutput.additionalContext` — injected text. It cannot add, remove, or reorder entries
in the Messages-API `tools` parameter. The best a hook could ever do is emit a **text nudge**
("tool X looks relevant, consider searching for it") — never a true retrieve-and-expose of a
tool definition the way `mr-hook` does for skills (where the Skill tool itself is always available
and the injected text is the actual selection signal).

### 2.5 Anthropic's own "custom tool search" extension point is a level up, not in scope

The API docs describe a supported way to swap in your own (e.g. embedding-based) tool-search
logic — implement a custom tool that returns `tool_reference` blocks in place of the built-in
regex/BM25 search. This is real prior art for "better ranking than BM25," but it operates at the
Messages-API / Agent SDK level, where the caller constructs the `tools` array and intercepts
tool-search calls directly. Claude Code CLI does not expose this seam to hooks or plugins — it owns
the `tools` array construction internally. Not reachable from meta-router's integration surface.

## 3. Options considered

| Option | Description | Verdict |
|---|---|---|
| A. Hook-based MCP tool retriever (the v1.5 hypothesis) | mr-hook discovers connected servers' tools, embeds them, injects top-k as `additionalContext` each prompt | **Infeasible as specified** — no corpus (2.3), no tool list visibility (2.2), can't affect tool availability anyway (2.4), and solves a problem that's already solved better, natively (2.1) |
| B. Personalized ToolSearch query nudge | A hook independently connects to each configured MCP server (from `~/.claude.json`), harvests `tools/list` itself, ranks by prompt similarity, and injects "try ToolSearch with query: X" | Technically buildable, but: duplicates server connections the harness already holds (cost/fragility), and the harness *already* emits a generic "search if relevant" nudge on every prompt with deferred tools present (confirmed 2.1) — this option only sharpens which keyword to use, a marginal gain for real engineering cost |
| C. Do nothing; rely on built-in ToolSearch | No new code | **Recommended** |

## 4. Go/No-Go gate

Ship v1.5 (in any form) only if **all** of the following become true — otherwise, no-go:

1. **A real gap is observed**, not hypothesized: concrete evidence (from `usage.jsonl`-style
   telemetry or direct observation) that Claude Code fails to call `ToolSearch` for a relevant MCP
   tool it should have found — i.e. the built-in nudge/search under-performs in practice, not just
   in theory.
2. **A static or cheaply-derivable corpus exists** for whatever is being retrieved — e.g. if a
   future Claude Code version starts writing a per-server tool-schema cache to disk (watch
   `~/.claude.json` and `~/.claude/` for a new field/file), this gate flips from "no corpus" to
   "corpus available," which is the single biggest blocker today.
3. **The integration point is real**, not text-nudge-only — either a new hook event ships that can
   pass a curated tool subset into the actual `tools` array, or Claude Code exposes the
   custom-tool-search seam (§2.5) to plugins/hooks rather than owning it internally.
4. **The marginal value clears the engineering cost** — Option B's own server-connection overhead
   and duplication vs. the harness's already-generic nudge must be shown to matter empirically
   (e.g. via an eval harness analogous to `mr-eval`/`mr-outcomes`), not assumed.

**Current status against the gate: 0 of 4 met.** Re-open this doc if a Claude Code release changes
§2.1–§2.5 (new hook fields, a persisted tool-schema cache, or a documented plugin seam into
`tools`/`tool_search`).

## 5. What NOT to build in the meantime

- Don't spawn independent MCP client connections from a hook to harvest `tools/list` — it
  duplicates the harness's own connections, adds fragility (auth, process lifecycle, port/pipe
  contention) and startup latency, for a nudge the harness already emits generically.
- Don't attempt to write to or shadow `~/.claude.json`'s `mcpServers` to cache schemas — that file
  is Claude Code's own config, not a corpus meta-router owns.
- Don't confuse this with v3 (headless multi-agent orchestration, already in progress via
  `mr-orchestrate`) — that's routing across *lanes* (claude/codex/GLM/local), an unrelated axis.

## 6. Sources

- [code.claude.com/docs/en/hooks.md](https://code.claude.com/docs/en/hooks.md) — hook event list,
  input/output schemas (fetched directly, quoted in §2.2).
- [platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool) —
  ToolSearch mechanics, `defer_loading`, custom client-side tool search (fetched directly, quoted
  in §2.1 and §2.5).
- Direct inspection of this machine's `~/.claude.json` (`mcpServers` shape) and
  `~/.claude/mcp-needs-auth-cache.json` (§2.3).
- This session's own system reminders and a live `ToolSearch("select:TodoWrite")` call (§2.1) —
  first-hand observation, not a research claim.
