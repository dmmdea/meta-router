#!/usr/bin/env node
// RS1/RS2 statusline tee — the sanctioned Claude quota signal.
// Tees the statusline stdin JSON (rate_limits.five_hour/seven_day) to
// ~/.meta-router/orchestrate/statusline-drop.json via atomic tmp+rename
// (quotasig reads it on every mr-orchestrate invocation), then delegates to
// the existing gsd statusline unchanged. Fail-open: a tee failure must never
// break the statusline.
//
// Install (the operator's keystroke, settings.json statusLine.command):
//   "C:/Program Files/nodejs/node.exe" "<HOME>/.meta-router/bin/mr-statusline-tee.js"
'use strict';
const fs = require('fs');
const path = require('path');
const os = require('os');
const { spawnSync } = require('child_process');

const DELEGATE = path.join(os.homedir(), '.claude', 'hooks', 'gsd-statusline.js');

let raw = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (d) => (raw += d));
process.stdin.on('end', () => {
  try {
    const dir = path.join(os.homedir(), '.meta-router', 'orchestrate');
    fs.mkdirSync(dir, { recursive: true });
    const tmp = path.join(dir, 'statusline-drop.json.tmp');
    fs.writeFileSync(tmp, raw);
    fs.renameSync(tmp, path.join(dir, 'statusline-drop.json'));
  } catch (e) {
    // fail-open
  }
  try {
    const r = spawnSync(process.execPath, [DELEGATE], {
      input: raw,
      stdio: ['pipe', 'inherit', 'inherit'],
    });
    process.exit(r.status || 0);
  } catch (e) {
    process.exit(0); // statusline must never hard-fail
  }
});
