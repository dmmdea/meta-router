# Design spec: v2 candidate — silent automatic model/tool routing via `PreToolUse`

Status: **NO-GO on model routing (hard, permanent). Narrow conditional-GO on one
tool-routing slice** (`PreToolUse` deny+redirect / `updatedInput` argument
rewrite) — buildable, not yet built, gated below.

**Naming note:** this is *not* the README's shipped "v2" (the `UserPromptSubmit`
offload nudge, live since v0.3.0). That name is taken. This doc reshapes what a
*next* hook-based capability tier could be, now that [v1.5](design-v1.5-tool-retrieval.md)
closed off MCP-tool-*retrieval* as infeasible. Treat the heading as "the next
candidate slice," not a rename of shipped work — a version-number decision is
separate from this research.

## 1. The hypothesis

v1.5 asked: can a hook *surface* (retrieve-and-inject) more MCP tool metadata,
mirroring the skill-surfacer pattern? Answer: no — no corpus, no tool-list
visibility, no lever to touch the `tools` array (see that doc, §2.2–2.4).

This doc asks a different, broader question: forget retrieval — can a hook
*silently and automatically route* a model or tool call, i.e. change what
actually executes without asking the user and without the routing decision
being a text suggestion Claude can ignore? The only hook event with any
decision-making power over a tool call is `PreToolUse` (it can allow/deny/ask
a call and, per finding 2.3 below, rewrite its arguments) — so that's the
event this doc investigates in full.

## 2. Findings

Verified directly against
[code.claude.com/docs/en/hooks.md](https://code.claude.com/docs/en/hooks.md)
(fetched fresh this session, not relayed from memory) and this machine's
installed CLI (`claude --version` → `2.1.207`, which post-dates every
version-gated feature cited below).

### 2.1 Model routing has zero hook surface — anywhere, not just `PreToolUse`

No hook event, `PreToolUse` included, carries a settable `model` field in its
output schema. The *input* side is just as closed: only `SessionStart` ever
receives a `model` field, it's read-only and not guaranteed present, and there
is no `$CLAUDE_MODEL` environment variable exposed to any hook. Model
selection happens above the hook layer entirely (`/model`, `--agent` startup
flags) and is fixed for the turn before `PreToolUse` ever fires. **This closes
"model routing via hooks" permanently, not just for v2** — it would take a new
Anthropic-side hook field to reopen, not a cleverer implementation.

### 2.2 `PreToolUse` cannot substitute a different tool or touch future tool availability

Confirmed from the decision-control table: `PreToolUse`'s only levers are
`permissionDecision` (`allow` / `deny` / `ask` / `defer`) and `updatedInput`.
There is no field to change `tool_name` — the tool Claude already chose is the
one that runs, or it's blocked outright. It also cannot add, remove, or
reorder entries in the `tools` array for subsequent calls; tool availability
is controlled only through the permissions system, skill enable/disable, and
MCP/plugin server configuration — none of which a hook can write to at
runtime. This matches v1.5 §2.4's finding for `UserPromptSubmit` and extends
it: no hook event, including the one with the most teeth, can redirect work
from one tool/server to a genuinely different one.

### 2.3 `updatedInput` is a real, undocumented-in-v1.5 mutation lever — scoped to same-tool arguments

This is new since the v1.5 doc (which characterized every hook's output as "a
text nudge... never a true retrieve-and-expose"). That claim holds for
`UserPromptSubmit`, but **not fully for `PreToolUse`**: `hookSpecificOutput.updatedInput`
"replaces a tool's arguments before it runs" (doc's own wording). A `PreToolUse`
hook matching e.g. `Bash` can rewrite `{"command": "rm -rf /tmp/build"}` to
`{"command": "npm run lint"}` before execution — same tool, different
arguments, no user prompt. `PermissionRequest` carries an equivalent
`decision.behavior: "allow"` + `updatedInput` shape.

Two things this can *not* do, confirmed by the doc having no example or
mention of either: (a) redirect to a different tool/server ("all examples
shown modify arguments of the same tool"), and (b) it's undocumented whether
Claude's own context reflects the original or the rewritten arguments — the
doc is silent on model-side transcript visibility of the rewrite. Treat that
as an open question requiring a live probe before relying on it, not an
assumed "fully silent to the model" guarantee.

### 2.4 Silence is real but asymmetric: silent to the user, not silent to the model

`permissionDecision: "deny"` (or exit code 2) suppresses the normal
interactive permission prompt — confirmed: "This prevents the tool call
without user intervention. The interactive prompt is suppressed." That's the
"silent" half of "silent automatic routing": no dialog interrupts the user.

But it is not silent to Claude: exit-2 stderr text (or `permissionDecisionReason`)
is fed back to Claude as the reason the call was blocked, specifically so
Claude can react to it — e.g. retry with a different approach or a different
tool of its own choosing. That's a *feature* for a routing use case (a deny
reason can name the preferred alternative tool by name and Claude can act on
the steer), but it means "automatic routing" here is closer to "automatic,
retryable enforcement with a steering reason" than a true invisible swap. Be
precise about this distinction when scoping — it's not the same claim as
`updatedInput`'s silent argument rewrite.

### 2.5 Matchers can target this precisely, including MCP server/tool patterns

`PreToolUse` matchers support exact tool names, `|`/`,` lists, and unanchored
JS regex, with first-class MCP support: `mcp__<server>__<tool>` naming, a
`.*` suffix to match every tool from a server (`mcp__local-offload__.*`), and
v2.1.195+ plugin-scoped MCP naming
(`mcp__plugin_<plugin>_<server>__<tool>`). An additional `if` field can filter
further by argument pattern (e.g. `Bash(git *)`, with subcommand- and
`$()`/backtick-aware matching). This is precise enough to target "calls to
tool X when a free local equivalent exists" without over-matching — the
missing piece the current v2 offload nudge doesn't have, since it fires on
prompt text before any tool is chosen.

## 3. Capability matrix

| Capability | Verdict | Lever |
|---|---|---|
| Route to a different **model** per call/turn | **No — permanent, no hook surface exists** | none |
| Substitute a **different tool** than the one Claude picked | **No** | none (`tool_name` immutable) |
| Filter/page which tools are **available** for future calls | **No** | none |
| Rewrite the **arguments** of the already-chosen tool, silently | **Yes** | `updatedInput` (same-tool only; model-visibility of the rewrite unverified) |
| Block a call and steer toward a named alternative, without a user prompt | **Yes** | `permissionDecision: "deny"` + `permissionDecisionReason` (silent to user, visible-by-design to Claude) |
| Target specific MCP tools/servers precisely | **Yes** | matcher regex incl. `mcp__server__.*`, plus `if` argument filter |

## 4. Reshaped scope

Given §3, "silent automatic model/tool routing" as originally framed is mostly
not achievable — model routing is a flat no, and tool routing is not a swap,
it's either an argument rewrite on the same tool or a deny-and-steer on the
same decision point. The reshaped, actually-buildable slice:

**A `PreToolUse` tool-call gate**, narrower than "routing": a new hook
registration (distinct from `mr-hook`'s existing `UserPromptSubmit` binary —
different event, different input/output schema, likely a second binary or a
`-mode=pretooluse` branch reusing the existing fail-open/deadline machinery)
that matches a small, explicit table of "tool X has a known free/local
equivalent Y" pairs and, on match:

- denies with `permissionDecisionReason` naming Y by tool name (the
  enforcement-grade version of the existing v2 nudge — this fires after
  Claude has already committed to calling X, catching cases the prompt-level
  text nudge missed or Claude ignored), or
- for pure argument-correction cases (not tool-swap cases), uses `updatedInput`
  to fix known-bad shapes before execution.

This is a genuinely new capability tier, not a retrofit of the shipped v2 —
it is an enforcement layer for tool-choice, layered *after* the existing
prompt-level suggestion, not a replacement for it. It complements, not
duplicates, both the shipped v2 (which fires before Claude even reasons about
tools) and the planned v3 orchestrator (which routes *external* headless
tasks across claude/codex/GLM/local lanes — a different axis entirely, unaffected
by anything in this doc).

## 5. Go/No-Go gate for the tool-call-gate slice

Ship only if all of the following hold — this mirrors v1.5's gate discipline
(evidence over hypothesis) rather than assuming the mechanism is worth
building just because it's technically possible:

1. **A concrete X→Y table exists and is small.** Start from real
   already-observed cases (e.g. a cloud MCP call where a `local-offload`
   equivalent exists) — not a speculative general-purpose mapping.
2. **The model-visibility question from §2.3 is resolved empirically** (a live
   probe: does a `PreToolUse`-rewritten `tool_input` show as the original or
   updated value in Claude's own transcript?) before `updatedInput` is used for
   anything beyond cosmetic fixes. If unresolved, ship deny+reason only
   (§2.4's lever), which has no such ambiguity.
3. **False-positive denials are measured near zero** on real usage before this
   goes live — a wrongly denied legitimate cloud-tool call is worse than the
   current soft nudge ever calling wrong, per the same precision-over-recall
   principle `mr-hook` already applies to its BM25 fallback gate.
4. **The marginal value over the existing v2 nudge is shown**, not assumed —
   i.e. evidence (from `usage.jsonl`/`outcomes.jsonl`-style telemetry) that
   Claude calls X *despite* the prompt-level nudge often enough that a
   stronger enforcement point pays for the added complexity.

**Current status against the gate: 0 of 4 met** (this doc is research only,
no table/probe/telemetry built yet). Model routing is not gated — it's simply
off the table until Anthropic ships a hook field for it.

## 6. What NOT to build

- Don't build any mechanism that assumes a hook can select a model — confirmed
  impossible (§2.1); wait for a platform change, don't work around it with a
  hack (e.g. hooks cannot spawn a different-model subagent invisibly either —
  subagent dispatch is a Claude-side decision, not hook-triggered).
- Don't use `updatedInput` to redirect between tools/servers — undocumented,
  unsupported, and every example in the spec is same-tool-only; treat
  cross-tool redirection as a deny+reason case, not a rewrite case.
- Don't ship `updatedInput`-based argument rewriting before resolving the
  model-visibility open question in gate item 2 — a silent rewrite Claude
  doesn't know happened risks it reasoning about a tool result that doesn't
  match the call it thinks it made.
- Don't conflate this with v3 (`mr-orchestrate`) — that routes external
  headless tasks across lanes via its own CLI/ledger, a completely different
  integration surface from an in-session `PreToolUse` hook.

## 7. Sources

- [code.claude.com/docs/en/hooks.md](https://code.claude.com/docs/en/hooks.md) —
  fetched fresh this session (two targeted passes): `PreToolUse` input/output
  schema, decision-control table, exit-code semantics, matcher syntax
  including MCP patterns, and the `updatedInput`/model-visibility/tool-substitution
  questions in §2.3–2.4.
- This machine's installed CLI version (`claude --version` → `2.1.207`),
  confirmed to post-date every version-gated feature cited (`prompt_id`
  v2.1.196+, hyphenated matcher names and plugin-scoped MCP naming v2.1.195+).
- [design-v1.5-tool-retrieval.md](design-v1.5-tool-retrieval.md) — prior
  research this doc extends rather than duplicates (retrieval vs. routing are
  distinct questions; §2.2/§2.4 findings there are reused, not re-derived,
  where they still apply to `UserPromptSubmit`).
