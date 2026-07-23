# Adjudication ledger — adversarial-review findings

Append-only. One row per finding batch or notable single finding.
Verdict ∈ fixed | declined | deferred (batch rows summarizing multiple
findings may use `mixed` severity; per-decline detail goes to
`declined-findings.md`). Cells must not contain the `|` character — the
structural canary parses on pipes; paraphrase code instead.

| date | pr | scope | finding | severity | verdict | reason |
|---|---|---|---|---|---|---|
| 2026-07-22 | #16 | v7 promotion gate | flagship guarantee Monte-Carlo-defeatable at n=5 (seed 372) | MAJOR | fixed | SignFlipP now enumerates 2^n exactly at small n |
| 2026-07-22 | #16 | v7 promotion gate | 12 further findings (anti-gaming, parity, counters) | mixed | fixed | all addressed; delta re-review verified |
| 2026-07-22 | #17 | e2 spend-down | 19 findings across two rounds (epoch latch, arm-needs-live-trace, throttle exclusion+freeze, receipt provenance) | mixed | fixed | all closed pre-merge |
| 2026-07-23 | #18 | b2 split scorecard | 4 MAJORs + 8 minors (neutral-state policy-input seeding, split-label validation, exact-p range) | mixed | fixed | all addressed pre-merge |
| 2026-07-23 | w8 | w8 scaffold | B1 regex missed LookupEnv + APIKEY spelling; unit test validated a divergent regex copy | MAJOR | fixed | pattern hoisted to shared B1Forbidden, extended; fixtures cover both accessors |
| 2026-07-23 | w8 | w8 scaffold | B11 canary ignored the CHANGELOG leg of its own invariant | MAJOR | fixed | canary now parses the top CHANGELOG heading and requires it to equal VERSION |
| 2026-07-23 | w8 | w8 scaffold | B5 verify pointer named router_test.go which has no override coverage | MINOR | fixed | repointed to fault_test.go (CONCEPT-CHANGE recorded in 0.9.0 entry) |
| 2026-07-23 | w8 | w8 scaffold | no positive test that a valid operator override outranks the compiled Seed table | MINOR | deferred | belongs in a router-package change; queue with the next W3/W6 router touch |
| 2026-07-23 | w8 | w8 scaffold | ledger canary evadable via leading space; pipe-in-cell breaks the 7-cell parse | MINOR | fixed | rows are TrimSpaced before validation; no-pipe rule documented above |
| 2026-07-23 | w8 | w8 scaffold | extend B11 parity to the six sibling cmd binaries | MINOR | declined | sibling dev tools are versioned independently by design; see declined-findings.md |
| 2026-07-23 | w8 | w8 scaffold | vendor dir unskipped; go list stderr swallowed; invariant bullets outside markers unhashed | NIT | fixed | vendor SkipDir; ExitError.Stderr surfaced; outside-marker B-bullets now fatal |
| 2026-07-23 | w3 | w3 policy zoo | -zoo composed silently with -live-quota, producing a guaranteed-trivial null | MINOR | fixed | the combination is now exit 2 |
| 2026-07-23 | w3 | w3 policy zoo | report ordering nondeterministic under the exact float ties -zoo creates | MINOR | fixed | SliceStable with policy-name tiebreak |
| 2026-07-23 | w3 | w3 policy zoo | assignCost gave unknown/abstain lanes cost 0, inverting the policyeval never-win-by-accident convention | MINOR | fixed | unknown lane now costs MaxInt32 |
| 2026-07-23 | w3 | w3 policy zoo | vacuous-vs-coincident null ambiguity in the artifact; dead Chars field; tie-break test message drift | NIT | fixed | diverged counts added to ZooEntry (settled the real run: 0/0); Chars dropped; message corrected |
