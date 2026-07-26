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
| 2026-07-25 | audit | 31-agent full-system audit | hook banner rendered EXPIRED windows as live pressure; deployed mr-hook 3 releases stale | CRITICAL | fixed | shared Bucket.Expired applied in the 2 unguarded readers; fleet check added; regression tests |
| 2026-07-25 | audit | 31-agent full-system audit | stale seeded statusline drop re-ingested as fresh provider truth on every route/run/mcp | MAJOR | fixed | freshness guard + observation-time stamping + source precedence; fixture quarantined (operator-approved) |
| 2026-07-25 | audit | 31-agent full-system audit | corrupt ledger silently destroyed on next write | MAJOR | fixed | UpdateChecked + quarantine-on-unmarshal-failure; all cmd writes surface the warning |
| 2026-07-25 | audit | 31-agent full-system audit | two armed weekly-replay tasks, 35 unattended cloud dispatches incl. 15 permission-skipping | CRITICAL | fixed | duplicate task unregistered (definition exported first); A2 disabled pending script fix + 1-task rehearsal (operator-approved) |
| 2026-07-25 | audit | 31-agent full-system audit | Aorus routes on the compiled Seed table (0.816x, non-inferior FALSE); retunes never promoted into seed.go | MAJOR | deferred | step 3 of the approved remediation order — next change after this PR |
| 2026-07-25 | audit | 31-agent full-system audit | A2 script PS 5.1 stderr semantics inverted; a2-alert.json has zero readers | MAJOR | deferred | task disabled so it cannot fire; fix + rehearsal queued behind step 3 |
| 2026-07-25 | audit | 31-agent full-system audit | GLM meters 1 unit/invocation vs per-request provider (429 at 21% local) ; one token shared across 2 hosts with machine-local guards | MAJOR | deferred | needs the operator's GLM posture decision (R1) before code |
| 2026-07-25 | audit | 31-agent full-system audit | GLM ships prompts+repo cwd to a third party with no sensitivity gate; same gate blocks W4 | MAJOR | deferred | queued after step 3; is also W4's blocker |
| 2026-07-25 | audit | 31-agent full-system audit | child processes inherit ambient env unscrubbed (ANTHROPIC_API_KEY etc. would redirect a headless claude to metered spend) | MAJOR | deferred | deny-list planned; no such var set on either host today |
| 2026-07-25 | audit | 31-agent full-system audit | B4/B7/B3/B5 verify pointers weaker than they read; B5 reopen trigger already fired | MINOR | deferred | W8 hardening pass; recorded so it cannot go stale again |
| 2026-07-25 | audit | 31-agent full-system audit | afelopez collaborator invitation EXPIRED ~2026-07-15; every "once Andres is added" item blocked on a dead trigger | MINOR | declined | operator-only action (re-invite); recorded in declined-findings for feed-forward |
| 2026-07-25 | egress | audit R3 | glm shipped prompt+repo cwd to a PRC-hosted provider with no sensitivity gate; same gate blocks W4 | MAJOR | fixed | deny-by-default egress gate, force-proof, exit 6, receipt-recorded; predicate lane-generic but enforcement per-adapter, so B14 canary fails any ungated third-party dispatcher |
| 2026-07-25 | egress | audit R4 | lane children inherited ambient env; an ANTHROPIC_API_KEY would redirect subscription dispatches to metered spend | MAJOR | fixed | first pass covered 2 of 5 sites (probe/verify/locallane still leaked); now all 9 spawn sites, mutation-verified caught at all 9 |
| 2026-07-25 | egress | audit R6/A2 | a2-alert.json had zero readers; PS5.1 stderr semantics killed the script; stale ok=true survived aborts | MAJOR | fixed | status reads a2_alert (BOM-tolerant); script rewritten with exit-code branching and failure-first alerts |
| 2026-07-25 | egress | rehearsal | PS5.1 -Encoding UTF8 writes a BOM that json.Valid rejects, silently dropping the alarm | MAJOR | fixed | script writes UTF-8 without BOM; reader trims a BOM defensively |
| 2026-07-25 | egress | audit R6 | A2 replay leg still never executed end to end | MAJOR | deferred | task remains DISABLED; 1-task rehearsal needs operator authorisation (spends quota) |
| 2026-07-25 | egress | review round 1 | empty cwd read as "no repo context" while os/exec inherits the parent dir — receipt certified prompt-only while exporting a client checkout | CRITICAL | fixed | egress.Plan resolves the inherited cwd and substitutes a neutral empty temp dir; prompt-only ENFORCED, live-proven |
| 2026-07-25 | egress | review round 1 | --add-dir inside --extra was a second, ungated export channel | MAJOR | fixed | egress.AddDirs extracts both forms; each gated before dispatch |
| 2026-07-25 | egress | self mutation-test | B13 accepted a COMMENT naming childenv.Scrub as proof of the call — deleting the call still passed | MAJOR | fixed | canary.StripGoComments (unit-tested both directions) + call-shaped regexes in B13/B14 |
| 2026-07-25 | egress | self mutation-test | B13's "process-control helper" heuristic keyed on "taskkill", exempting claudelane/codexlane/locallane — the three most important spawn sites | MAJOR | fixed | explicit one-entry allowlist with a stated reason; mutation-verified across all 9 sites |
| 2026-07-25 | egress | self mutation-test | B14 keyed its function-body end on a bare closing-brace line and would go inert on a CRLF fresh clone | MAJOR | fixed | StripGoComments normalizes CRLF; mutation-tested in both LF and CRLF shapes |
| 2026-07-25 | egress | review round 1 | CHANGELOG claimed free lanes "inherit" the egress gate when one adapter called it, and R4 "fixed" while 3 sites leaked | MINOR | fixed | both entries corrected with the real scope; claim replaced by the B14 canary |
| 2026-07-25 | egress | review round 1 | a2_alert had no staleness guard: a frozen ok=true renders like a fresh pass | MAJOR | fixed | a2_watch_stale ages the verdict (>8d), and reports un-timestamped and future-stamped verdicts |
| 2026-07-25 | egress | concept gate | B13 canary shipped before its Bible invariant existed | MINOR | fixed | B13+B14 added to ROUTER_BIBLE.md, bible.sum updated, CONCEPT-CHANGE recorded in the CHANGELOG |
