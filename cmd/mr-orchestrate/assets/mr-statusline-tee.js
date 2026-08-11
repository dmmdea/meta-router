#!/usr/bin/env node
// RS1/RS2 statusline tee — the sanctioned Claude quota signal.
//
// Tees the statusline stdin JSON (rate_limits.five_hour/seven_day) to
// <state>/statusline-drop.json via atomic tmp+rename (quotasig reads it on
// every mr-orchestrate invocation), then delegates to whatever statusline the
// host was already running, unchanged.
//
// Fail-open in both halves: a tee failure must never break the statusline, and
// a delegate failure must never take the session down with it.
//
// This file is EMBEDDED in mr-orchestrate and written out by
// `mr-orchestrate install claude`, which also records the statusline command it
// wrapped in <state>/statusline-delegate.json. Editing the copy under
// ~/.meta-router/bin only lasts until the next install; edit the repo copy.
'use strict';
const fs = require('fs');
const path = require('path');
const os = require('os');
const { spawnSync } = require('child_process');

// The state dir, in order of authority: the argument the installer baked into
// the statusLine command (it knows which home it wired), then MR_ORCH_STATE,
// then the default. Argv first because the host process need not carry the
// environment variable, and a tee that resolves a different dir than the
// installer wrote to finds no delegate and prints nothing.
const STATE =
  process.argv[2] ||
  process.env.MR_ORCH_STATE ||
  path.join(os.homedir(), '.meta-router', 'orchestrate');

// The statusline must never hard-fail, but it must not fail SILENTLY either:
// every mute catch here was a failure mode an operator could not see. stderr is
// visible under `claude --debug` and costs nothing otherwise.
function warn(what, err) {
  try {
    process.stderr.write('mr-statusline-tee: ' + what + ': ' + err + '\n');
  } catch (e) {
    /* nothing left to do */
  }
}

// The command this tee wraps. Written by the installer from whatever
// statusLine.command it replaced, so the operator's own statusline keeps
// rendering. An install refuses to wire the tee when there is nothing to wrap,
// so an empty delegate here means the file was hand-edited — render nothing
// rather than guess at a command.
function delegateCommand() {
  try {
    const raw = fs.readFileSync(path.join(STATE, 'statusline-delegate.json'), 'utf8');
    const d = JSON.parse(raw);
    if (d && typeof d.command === 'string' && d.command.trim() !== '') return d.command;
    warn('delegate file has no command', JSON.stringify(d));
  } catch (e) {
    warn('cannot read the delegate file', e);
  }
  return '';
}

let raw = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (d) => (raw += d));
process.stdin.on('end', () => {
  // Only a payload that PARSES replaces the drop. A blank or truncated render
  // would otherwise overwrite a good drop with a 0-byte file carrying a fresh
  // mtime — which defeats quotasig's staleness check, so the router would read
  // "no quota signal" as "current".
  let ok = false;
  try {
    JSON.parse(raw);
    ok = true;
  } catch (e) {
    warn('statusline payload is not JSON, keeping the previous drop', e);
  }
  if (ok) {
    try {
      fs.mkdirSync(STATE, { recursive: true });
      const tmp = path.join(STATE, 'statusline-drop.json.tmp');
      fs.writeFileSync(tmp, raw);
      fs.renameSync(tmp, path.join(STATE, 'statusline-drop.json'));
    } catch (e) {
      warn('cannot write the quota drop', e); // fail-open: the statusline is not
    }
  }
  const delegate = delegateCommand();
  if (delegate === '') process.exit(0);
  try {
    // shell:true because the delegate is a COMMAND LINE as the host config
    // spelled it (interpreter + quoted script path + flags), not an argv array.
    // It crosses no new trust boundary: this is the operator's OWN
    // statusLine.command, a string the host already shell-executes every render,
    // copied verbatim by the installer. Splitting it into argv here would mean
    // re-implementing Windows command-line quoting, which is a worse bug class
    // than the one the rule warns about.
    const r = spawnSync(delegate, {
      shell: true,
      input: raw,
      stdio: ['pipe', 'inherit', 'inherit'],
    });
    if (r.error) warn('delegate did not run', r.error);
    // r.status is null when the child died on a signal; `|| 0` would report
    // that as success. Anything that is not a number is a failure we did not
    // get an exit code for.
    process.exit(typeof r.status === 'number' ? r.status : 0);
  } catch (e) {
    warn('delegate spawn failed', e);
    process.exit(0); // statusline must never hard-fail
  }
});
