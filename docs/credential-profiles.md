# Credential profiles (multi-account) — W2

meta-router can dispatch across multiple credential SUBJECTS (accounts) of the
same lane — e.g. two Claude subscriptions — routing each headless dispatch to
the healthiest account and rotating only on typed vendor limits. Interactive
sessions are unaffected: they run on whatever account they launched under; this
is headless-dispatch seamlessness (DG-1).

## What it does and does not do

- **Does:** picks the best provisioned subject per dispatch (open > throttled >
  exhausted, then higher pace headroom, then your registry order), points that
  dispatch at the account's isolated auth home, accounts its usage to that
  account's windows, and records rotation provenance on the receipt.
- **Never:** creates, copies, or refreshes credentials; rotates on a network
  error (only on typed vendor-limit signals or measured exhaustion); touches
  your live `~/.claude` / `~/.codex` as anything but the default subject.

Single-account machines need no registry and behave exactly as before.

## Provisioning a second Claude account (two commands)

1. Log the second account into an isolated home (one time):

   ```
   set CLAUDE_CONFIG_DIR=C:\Users\<you>\.meta-router\profiles\claude-acct2
   claude /login
   ```

2. Register it (`~/.meta-router/orchestrate/profiles.json`):

   ```json
   {
     "claude": [
       {"subject": "default", "home": ""},
       {"subject": "acct2", "home": "C:/Users/<you>/.meta-router/profiles/claude-acct2"}
     ]
   }
   ```

`home: ""` means the CLI's own live location. Registry ORDER is your
preference — the first entry is used unless it is throttled/exhausted (R15).

Verify: `mr-orchestrate profiles` lists each subject's provisioning state,
admission state, and pace slack; an un-provisioned profile shows the exact
login command to run. `mr-orchestrate status` shows per-account windows under
each lane's `subjects` block once more than one profile exists.

Codex second accounts use the same registry with `home` pointing at a
`CODEX_HOME` the operator provisioned with `codex login` — wired for the
future; the claude dual-account path is the one exercised today.
