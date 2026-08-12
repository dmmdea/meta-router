# ROUTER_BIBLE — meta-router invariants

The concept-level laws of this router. Every invariant carries a `verify:`
pointer: a canary/test name, a package path whose suite pins it, or `process`
(enforced by review protocol, not code). The invariants block below is hashed
into `docs/bible.sum` — changing it without the CONCEPT-CHANGE protocol fails
`TestCanaryBibleHash`.

## CONCEPT-CHANGE protocol

Amending any invariant (or what a canary pins) requires, in ONE PR:
1. The invariant edit here, with the rationale in the PR body.
2. `docs/bible.sum` regenerated (the failing canary prints the new hash).
3. A `CONCEPT-CHANGE:` line in that version's CHANGELOG entry naming the
   invariant (e.g. `CONCEPT-CHANGE: B1 amended — free lane class (DG-2)`).
A concept change that arrives as a quiet test edit is a review-blocking defect.

<!-- invariants:begin -->
- **B1 — Subscription-auth only.** No lane adapter authenticates with an API
  key (`*_API_KEY` env, `x-api-key` header). A structurally-zero-spend free
  lane class (ToS-clean subset) enters ONLY as an explicit CONCEPT-CHANGE that
  scopes the canary's allowlist. verify: `TestCanaryB1NoAPIKeyAuth`
- **B2 — Deterministic, LLM-free hot path.** `route` decides by rank table +
  admission mask; its package closure has no network or subprocess capability.
  verify: `TestCanaryB2RouterPurity`
- **B3 — Non-inferiority margin 0.15, floored, never widened.** Every
  promotion verdict is read against the pre-registered margin. verify: `TestCanaryB3MarginFloor`
- **B4 — Nothing burns windows unmetered.** Every dispatch passes admission +
  ledger accounting; fault paths degrade to refusal, never to unmetered spend.
  verify: `internal/orch/dispatch`
- **B5 — Operator overrides outrank every autonomous signal.** The rank-table
  override file, config priors, and kill-switches win over learned/derived
  state. verify: `internal/orch/router/fault_test.go`
- **B6 — Unknown cells are counted, never imputed.** A missing oracle cell is
  a hole in the report, never a guessed number. verify: `internal/policyeval/policyeval_test.go`
- **B7 — oracle-best is a ceiling, never a deployable claim.** In split mode
  it is marked in_sample and its verdict suppressed. verify: `cmd/mr-scorecard/main.go`
- **B8 — Eval before promotion.** Every routing-visible change clears the
  B'2 `-split` cross-validated gate (tuned on tuning, verdict on heldout)
  before it ships as default behavior. verify: `process`
- **B9 — PR-only ships.** Main moves only by PR + merge commit; docs and
  CHANGELOG move in the same PR as the change. verify: `process`
- **B10 — Single binary, no resident server.** MCP is stdio; no daemon, no
  web server in the dispatch path. verify: `process`
- **B11 — Version parity.** `VERSION`, the orchestrator's `version` var, and
  the CHANGELOG's top entry move together. Scope: the deployed orchestrator
  binary is the pinned surface; sibling dev tools under `cmd/` are versioned
  independently by design. verify: `TestCanaryB11VersionParity`
- **B12 — Complexity ratchet.** Non-test Go LOC stays under the committed
  budget; raising it is a conscious, reviewed act. verify: `TestCanaryB12ComplexityRatchet`
- **B13 — Every spawn that could be a model lane gets a scrubbed environment.**
  An ambient `ANTHROPIC_API_KEY` is honoured unconditionally by headless Claude
  Code, ahead of OAuth, which turns a "subscription" dispatch into metered spend
  while the receipt still reads `cash_usd: 0` (R10). Checked PER SITE and
  structurally: one scrub in a file does not cover a second spawn in it, and a
  later `cmd.Env` assignment that does not re-derive from the scrub undoes it.
  The only exemption is a spawn whose argv[0] is a string literal naming a
  non-lane program (`git`, `taskkill`, …) — a VARIABLE command could resolve to
  a model binary and is never exempt. Scope is deliberately "could be a model
  lane", not "every process": `git` inheriting `PATH` is not a billing risk, and
  an invariant stated wider than it is enforced is a false invariant. Checked per
  SITE in statement order, so a later rebind of the variable, an earlier spawn,
  and a sibling block each get their own verdict rather than sharing one.
  verify: `TestCanaryB13EverySpawnScrubsEnv`
- **B14 — Every selectable third-party lane has a dispatcher that reaches the
  egress gate.** The gate's predicate is lane-generic but enforcement is NOT
  inherited — an adapter that never calls `egress.Plan` is ungated and exports
  whatever cwd it runs in. Seating a new free lane means wiring the gate, not
  trusting it. B14 is a TRIPWIRE for lanes not yet written; it asserts the call
  exists, which a mutation can satisfy while still leaking. Obedience is proved
  behaviourally on `effective_cwd` and the exit code.
  verify: `TestCanaryB14ThirdPartyLanesAreGated`, `cmd/mr-orchestrate/egress_dispatch_test.go`
- **B15 — Every ranked `(lane, model)` pair has oracle evidence.** Routing
  evidence is keyed by the CELL `(lane, model, effort)`, so a rank-table entry
  naming a model nobody measured is a recommendation with nothing behind it —
  and pooling it with a sibling model's history is exactly the blending the
  cell exists to end (B6: unknown counted, never imputed). The gate is at the
  MODEL level deliberately: every legacy row predates effort capture and
  carries `effort: unrecorded`, so gating the full key today would flag all 18
  ranked configs and isolate nothing. Full-cell coverage ships as
  `config_coverage` in the scorecard — reported now, gated once the
  re-baseline fills it. Deferred rows are holes, never evidence.
  verify: `TestCanaryB15RankedModelsHaveEvidence`
<!-- invariants:end -->

## Review protocol (W8)

- **Adjudication ledger** (`docs/reviews/adjudication-ledger.md`): every
  adversarial-review finding on a PR lands there with verdict
  `fixed|declined|deferred` and a reason. Append-only.
- **Declined-findings feedback** (`docs/reviews/declined-findings.md`): the
  standing digest of adjudicated-DECLINED findings. Every fresh-context
  adversarial review prompt MUST include this file, instructing the reviewer
  not to re-raise them without new evidence.
- **Reviewer liveness floor:** any LLM-judge or review step must demonstrate
  liveness on its first use in a session (e.g. a planted-defect smoke or a
  non-empty finding on a known-dirty diff) before its clean verdict counts.
  A reviewer that has never found anything is unmeasured, not passing.
