# Contributing to meta-router

Thanks for your interest in improving meta-router: a local skill surfacer for Claude Code
(`mr-index`, `mr-hook`) and an opt-in multi-lane orchestrator (`mr-orchestrate`) with its
evaluation tools. Contributions of all sizes are welcome.

## Build and check

One Go module with no third-party dependencies and ten commands under `cmd/`. From the
repository root:

```bash
go build ./...
go vet ./...
go test ./... -count=1
python scripts/ci/privacy-gate.py --range origin/main..HEAD
```

All four must pass before you open a pull request. The tests need no embedding endpoint and
no vendor account.

## What a change must carry

`ROUTER_BIBLE.md` holds the invariants. The ones a contributor meets first:

- **Fail-open hook.** `mr-hook` never blocks or breaks a prompt: every error path surfaces
  nothing and exits 0.
- **Version parity (B11).** A release moves `VERSION`, the `version` variable in
  `cmd/mr-orchestrate/main.go` and the top `CHANGELOG.md` entry together.
- **Changelog in the same PR (B9).** Say what changed and what you measured.
- **Complexity ratchet (B12).** Non-test Go lines stay under `docs/complexity-budget.json`. If
  your change adds lines, raise the budget to the measured total in the same PR and say why in
  its `_note`.
- **Privacy gate.** No real names, hostnames, home paths, e-mail addresses or IP addresses in
  code, tests, commit messages or PR text (`docs/privacy-gate.md`).
- **Routing changes are measured first (B8, B15).** A change to the rank table needs evidence
  from the evaluation tools, not an argument.

## Guidelines

- **Keep changes scoped.** One focused change per PR; no drive-by refactors or renames.
- **Match the existing style.** Run `gofmt` on the files you touch.
- **Tests are for behavior that can break.** Add one when it guards a defect that happened, or
  a security, money or data contract, and drive it through a public interface. Retrieval
  changes are validated with `mr-eval -goldset testdata/goldset.jsonl`.
- **Commit messages** say what changed and why, in the imperative.

## Opening a pull request

1. Fork the repo and create a branch off `main`.
2. Make your change with the four checks above green.
3. Open a PR describing what changed, why, and the real run that shows it works.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
