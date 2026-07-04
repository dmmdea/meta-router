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
