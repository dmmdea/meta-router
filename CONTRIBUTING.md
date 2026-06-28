# Contributing to meta-router

Thanks for your interest in improving meta-router — a fully local capability router that
surfaces the right Claude Code skills for each prompt. Contributions of all sizes are welcome.

## Build & test

This is a single-module Go project with two binaries under `cmd/`. From the repository root:

```bash
go build ./...   # build mr-hook, mr-index, mr-eval
go vet ./...     # static analysis — keep this clean
go test ./...    # run the full test suite
```

All three must pass before you open a pull request. The tests do not require a running
embedding endpoint.

## Guidelines

- **Keep changes scoped.** One focused change per PR. Avoid drive-by refactors, renames, or
  reformatting unrelated code — they make review harder and bury the actual change.
- **Match the existing style.** Run `gofmt` (or `go fmt ./...`) before committing.
- **Add or update tests** for any behavior you change. If you touch ranking, validate it with
  `mr-eval -goldset testdata/goldset.jsonl`.
- **Conventional-ish commit messages.** Prefix with the kind of change — `feat:`, `fix:`,
  `docs:`, `test:`, `refactor:`, `chore:` — followed by a short imperative summary.
- **Stay fail-open.** The hook must never block or break a prompt; any error path resolves to
  "surface nothing, exit 0."

## Opening a pull request

1. Fork the repo and create a branch off `main`.
2. Make your change, with `go build ./...`, `go vet ./...`, and `go test ./...` all green.
3. Open a PR describing what changed and why. Link any related issue.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
