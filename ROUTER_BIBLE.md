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
