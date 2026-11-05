package main

import (
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
)

// holdoutArm reports whether a session belongs to the holdout arm: its skill list is computed
// and logged but not injected, so the surfaced-then-invoked rate can be compared with the
// sessions that were shown it. The assignment is per SESSION (a hint stays in context for the
// rest of a session, so a per-prompt split would contaminate the control) and a pure function
// of the session id, so every prompt of a session lands in the same arm with no stored state.
// No id means no stable arm, so such a payload is never held out.
func holdoutArm(sessionID string, pct int) bool {
	if pct <= 0 || sessionID == "" {
		return false
	}
	if pct >= 100 {
		return true
	}
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	return int(h.Sum32()%100) < pct
}

// hookConfig is the per-machine file ~/.meta-router/hook.json. The two knobs live here, not in
// the shared settings.json arguments: a hook binary older than the one that learned to parse
// fail-open exits 2 on a flag it does not know, exit 2 blocks the prompt, and a machine that
// pulled shared settings before its binary would lose every prompt.
type hookConfig struct {
	MaxPromptLen int `json:"max_prompt_len"`
	HoldoutPct   int `json:"holdout_pct"`
}

// hookConfigPath is hook.json beside the usage log this run writes to, or "" when there is no
// log path. Following the log keeps a run pointed at a scratch log (every test, any dry run)
// from picking up the machine's real knobs: a holdout arm or a length cap read from the
// developer's own ~/.meta-router would make the suite pass on one machine and fail on the next.
func hookConfigPath(logPath string) string {
	if logPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(logPath), "hook.json")
}

// readHookConfig reads the per-machine file. A missing or unreadable file is no configuration
// and no error; bad JSON is no configuration and an error the row records.
func readHookConfig(path string) (hookConfig, error) {
	var c hookConfig
	if path == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return hookConfig{}, err
	}
	return c, nil
}
