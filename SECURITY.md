# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report them privately using GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
(the **"Report a vulnerability"** button on the repository's **Security** tab). Include a
description of the issue, the affected version, and steps to reproduce.

We will acknowledge your report and work with you on a fix and coordinated disclosure.

## Security model

meta-router is designed to be safe by default:

- **It runs entirely on your machine.** Prompt text is sent only to the local embedding
  endpoint you configure — never to any third-party or cloud service.
- **No raw prompts are persisted.** The usage log (`~/.meta-router/usage.jsonl`) stores only a
  SHA-256 hash of the prompt plus its length — never the raw text.
- **Fail-open by design.** On any error, timeout, or cold embedder the hook surfaces nothing
  and exits cleanly, so it can never block or break a prompt.
- **It does not edit your `settings.json`.** Registering and removing the hooks is always your
  explicit action.

The only network call meta-router makes is to the local embedding endpoint you point it at.
