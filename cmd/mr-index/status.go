package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// refreshStatus is one line of ~/.meta-router/refresh.log — written on EVERY
// refresh run, success or failure, so a silent SessionStart hook leaves an
// auditable trail.
type refreshStatus struct {
	Ts            string `json:"ts"`
	TsUnix        int64  `json:"ts_unix"`
	EntriesBefore int    `json:"entries_before"`
	EntriesAfter  int    `json:"entries_after"`
	Added         int    `json:"added"`
	Removed       int    `json:"removed"`
	Reembedded    int    `json:"reembedded"`
	DurationMs    int64  `json:"duration_ms"`
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	Forced        bool   `json:"forced,omitempty"`
	// Identity is the index's recorded model/template identity after this
	// run (or the one it refused on). Without it refresh.log is
	// identity-blind: a fresh rebuild after a templated index went missing
	// silently reverts the deployment to raw, and no row anywhere records
	// the flip (review 2026-08-16).
	Identity string `json:"identity,omitempty"`
	// StaleRoots records roots.json entries whose path no longer exists. It is
	// here rather than only on stderr because refresh runs from the
	// SessionStart hook with nobody watching stderr — refresh.log is the only
	// channel that actually reaches a human, and it is where the 155→112 index
	// decay was eventually diagnosed from.
	StaleRoots []string `json:"stale_roots,omitempty"`
	// Noop: the harvest matched the index exactly, so nothing was written and
	// no backup rotated. Skipped: why the run did nothing at all ("locked" =
	// another refresh holds the lock). RepairedFrom: the index.json could not
	// be parsed and this run rebuilt it from "bin-stale" (the sidecar) or "bak".
	Noop         bool   `json:"noop,omitempty"`
	Skipped      string `json:"skipped,omitempty"`
	RepairedFrom string `json:"repaired_from,omitempty"`
}

func appendRefreshStatus(path string, st refreshStatus) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	line, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}
