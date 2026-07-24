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
| 2026-07-23 | w1 | w1 quota truth | HTTP polls ran INSIDE the ledger write lock — 40s worst case vs the 30s lock-steal, reintroducing the cross-process race Update prevents | MAJOR | fixed | network half hoisted out (fetchPolls before Update; applyPolls inside a sub-second txn) |
| 2026-07-23 | w1 | w1 quota truth | scoped-alert latch wiped by every non-polling status run (unconditional writeScopedAlert with empty set) | MAJOR | fixed | latch touched only on a SUCCESSFUL claude fetch; skipped or failed polls leave it alone |
| 2026-07-23 | w1 | w1 quota truth | poll stamps saved after failed txns clobbered fresh stamps and defeated the rate limit | MINOR | fixed | finishPolls runs only on a committed transaction |
| 2026-07-23 | w1 | w1 quota truth | redactErr advertised but never called; wham capture wrote unsanitized account identifiers; parity report map-ordered; is_active dropped from scoped alerts | mixed | fixed | redactErr wired at the transport-error path; capture sanitized (idFields extended); parity keys sorted; is_active passed through unfiltered |
| 2026-07-23 | w1 | w1 quota truth | pace.Binding is subject-blind (W2 latent, not a current defect) | NIT | deferred | recorded in the Binding doc comment; W2's subject-aware callers must filter by Subject |
| 2026-07-23 | w2 | w2 credential profiles | single-profile poll path stored literal subject "default" → leaked subject field into status/ledger/trace JSON (byte-identical violation) | MAJOR | fixed | ledger.get + ApplySnapshotsSubject canonicalize "default"→""; negative-assertion regression test added |
| 2026-07-23 | w2 | w2 credential profiles | poll subcommand hard-failed on a malformed registry while status/run fail-open | MINOR | fixed | poll now warns + degrades to default subject |
| 2026-07-23 | w2 | w2 credential profiles | legacy poll stamps never omitted (zero time.Time + omitempty); rotation_from named ps[0] not the eligible incumbent | NIT | fixed | legacy stamps now *time.Time (nil omits); Select returns firstEligible, receipt records it |
