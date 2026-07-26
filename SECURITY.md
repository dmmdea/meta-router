# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report them privately using GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
(the **"Report a vulnerability"** button on the repository's **Security** tab). Include a
description of the issue, the affected version, and steps to reproduce.

We will acknowledge your report and work with you on a fix and coordinated disclosure.

## Security model

This repo ships **two** tools with different data boundaries. Read the one you run.

### The skill surfacer (`mr-index` / `mr-hook`)

- **It runs entirely on your machine.** Prompt text is sent only to the local embedding
  endpoint you configure — never to any third-party or cloud service.
- **No raw prompts are persisted.** The usage log (`~/.meta-router/usage.jsonl`) stores only a
  SHA-256 hash of the prompt plus its length — never the raw text.
- **Fail-open by design.** On any error, timeout, or cold embedder the hook surfaces nothing
  and exits cleanly, so it can never block or break a prompt.
- **It does not edit your `settings.json`.** Registering and removing the hooks is always your
  explicit action.

The only network call the surfacer makes is to the local embedding endpoint you point it at.

### The orchestrator (`mr-orchestrate`)

The orchestrator's whole job is to **dispatch your prompt to another agent CLI**, so it does
send data off the machine. Being explicit about that is the point of this section.

- **Where prompts go.** A dispatch runs the lane's own CLI as a child process: `claude`
  (Anthropic), `codex` (OpenAI), `glm` (Z.ai — PRC-hosted), or a local model, chosen by the
  router. Your prompt goes wherever that lane's provider is.
- **Repository context is gated for third-party lanes.** Any lane outside your own
  Anthropic/OpenAI subscriptions (currently `glm`, and the seated-but-unshipped
  `groq`/`cloudflare`/`gemini`/`nim`) is **deny-by-default** for repository context: a
  dispatch may only run inside a directory you have explicitly allowlisted in
  `glm_allow_repos`. Entries must be **absolute** — a relative entry would resolve against
  whatever directory the orchestrator happens to be in, so `"."` would self-allowlist every
  checkout in turn, and it is refused. An unresolvable path, a link or junction whose target
  leaves the allowlisted tree, and an empty allowlist all **fail closed**, and `--force` does
  not bypass it — `--force` overrides quota judgement, never a data export.
  - Because `os/exec` runs a child in the parent's directory, "no working directory" is not
    the same as "no repository context". When the inherited directory is not allowlisted, the
    dispatch is run in a **neutral empty directory** so prompt-only is enforced, not assumed.
  - **`--extra` is gated on the same footing as the working directory**, because it is
    forwarded verbatim to the child. Values of path-bearing flags are checked against the
    allowlist, including the *whole* value list of a variadic flag such as
    `--add-dir <directories...>` or `--mcp-config <configs...>`. Any token the gate cannot
    account for — an unrecognized flag, a bare positional — is **refused**, on the reasoning
    that an unmodelled flag may carry a path and the CLI's flag set grows over time.
  - `egress_prompt_only_denied: true` closes the **prompt-only** path, i.e. dispatches that
    carry no repository context at all. It is a tightening of that one case, **not** a master
    switch: with a repo allowlisted, dispatches inside it still run. To close a third-party
    lane entirely, empty its allowlist and set this flag, or remove the lane from routing.
  - Every dispatch records its gate decision and basis on the receipt (`egress_gate`) —
    including quota deferrals and paced deferrals, which are decided after the gate has
    already ruled — so an export is always countable after the fact.
- **Credentials are never handed to a child by accident.** Lane children get a **scrubbed**
  environment: API keys, auth tokens, base-URL and cloud-routing overrides, OAuth token file
  descriptors and model pins are removed from the ambient environment before each lane appends
  the credentials it deliberately uses. An ambient `ANTHROPIC_API_KEY` is otherwise honoured by
  headless Claude Code ahead of OAuth, silently converting a subscription dispatch into metered
  API spend.
- **Receipts, not prompts.** `~/.meta-router/orchestrate/dispatch.jsonl` records routing
  decisions, quota accounting and gate verdicts. Nothing operator-specific lives in the repo.
