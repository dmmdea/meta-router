# Adjudication ledger — adversarial-review findings

Append-only. One row per finding batch or notable single finding.
Verdict ∈ fixed | declined | deferred (batch rows summarizing multiple
findings may use `mixed` severity; per-decline detail goes to
`declined-findings.md`).

| date | pr | scope | finding | severity | verdict | reason |
|---|---|---|---|---|---|---|
| 2026-07-22 | #16 | v7 promotion gate | flagship guarantee Monte-Carlo-defeatable at n=5 (seed 372) | MAJOR | fixed | SignFlipP now enumerates 2^n exactly at small n |
| 2026-07-22 | #16 | v7 promotion gate | 12 further findings (anti-gaming, parity, counters) | mixed | fixed | all addressed; delta re-review verified |
| 2026-07-22 | #17 | e2 spend-down | 19 findings across two rounds (epoch latch, arm-needs-live-trace, throttle exclusion+freeze, receipt provenance) | mixed | fixed | all closed pre-merge |
| 2026-07-23 | #18 | b2 split scorecard | 4 MAJORs + 8 minors (neutral-state policy-input seeding, split-label validation, exact-p range) | mixed | fixed | all addressed pre-merge |
