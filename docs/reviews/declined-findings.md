# Declined findings — review feed-forward digest

Findings adjudicated DECLINED. Paste this file into every fresh-context
adversarial review prompt: do not re-raise these without NEW evidence.

## 2026-07-23 — w8 scaffold review

**"Extend B11 version parity to all `cmd/*` binaries"** (mr-scorecard,
mr-eval, mr-goldreplay, mr-goldverify, mr-verifier, mr-promote carry
independent version consts). DECLINED: those are non-deployed dev tools;
pinning them to the product version would force meaningless lockstep bumps.
B11's scope is documented in the Bible ("deployed orchestrator binary is the
pinned surface"). REOPENS IF: any sibling binary becomes a deployed/installed
artifact — then it joins the parity canary.
