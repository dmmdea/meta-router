#!/usr/bin/env bash
# Deploy the mr-* fleet with build provenance, then verify it.
#
# Every binary is stamped with -ldflags "-X main.buildRev=<HEAD>" because Go
# omits AUTOMATIC vcs stamping for git-worktree builds — which is how this
# project builds — so relying on it left `mr-orchestrate fleet` unable to tell a
# current binary from a 6-day-stale one (audit 2026-07-25: an admission fix sat
# undeployed through three releases while the per-prompt quota banner reported
# expired windows as live throttle pressure).
#
# Usage: scripts/deploy-fleet.sh [bin-dir]   (default ~/.meta-router/bin)
set -euo pipefail
BIN="${1:-$HOME/.meta-router/bin}"
REV="$(git rev-parse HEAD)"
mkdir -p "$BIN"
for c in mr-orchestrate mr-hook mr-index mr-scorecard mr-goldreplay mr-goldverify mr-promote; do
  [ -d "./cmd/$c" ] || continue
  go build -ldflags "-X main.buildRev=$REV" -o "$BIN/$c.exe" "./cmd/$c"
  echo "built $c @ ${REV:0:12}"
done
# Verify: -strict fails on anything stale, dirty, unstamped or unreadable.
"$BIN/mr-orchestrate.exe" fleet -expect "$REV" -strict
