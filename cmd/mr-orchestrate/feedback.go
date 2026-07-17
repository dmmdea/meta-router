package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// runFeedback implements `mr-orchestrate feedback <ts> good|bad` (S2R-9): tag
// one dispatch receipt with an operator quality verdict so slice-4 replay and
// gold-set harvesting have labels. <ts> is an RFC3339 prefix that must match
// exactly one receipt (disambiguate by lengthening it).
func runFeedback(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: mr-orchestrate feedback <ts-prefix> good|bad")
	}
	raw, err := os.ReadFile(dispatchPath())
	if err != nil {
		return fmt.Errorf("dispatch log unreadable (nothing to tag yet?): %w", err)
	}
	out, err := applyFeedback(raw, args[0], args[1])
	if err != nil {
		return err
	}
	// Atomic replace, per-PID tmp (the ledger.Save pattern): the receipt log
	// is an asset — a torn rewrite would cost the whole replay substrate.
	tmp := fmt.Sprintf("%s.tmp.%d", dispatchPath(), os.Getpid())
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dispatchPath()); err != nil {
		return err
	}
	fmt.Printf("feedback: receipt %s* tagged quality=%s\n", args[0], args[1])
	return nil
}

// applyFeedback is the pure core: set quality on the single receipt whose ts
// matches the prefix. Unmatched lines pass through BYTE-identical (fields a
// future Record version adds must never be re-marshalled away here).
func applyFeedback(log []byte, tsPrefix, verdict string) ([]byte, error) {
	if verdict != "good" && verdict != "bad" {
		return nil, fmt.Errorf("verdict must be good|bad, got %q", verdict)
	}
	lines := bytes.Split(log, []byte("\n"))
	matched := -1
	count := 0
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue // a corrupt line is skipped, never fatal (fail-open)
		}
		if ts, _ := m["ts"].(string); len(ts) >= len(tsPrefix) && ts[:len(tsPrefix)] == tsPrefix {
			matched = i
			count++
		}
	}
	switch {
	case count == 0:
		return nil, fmt.Errorf("no receipt with ts prefix %q in %s", tsPrefix, dispatchPath())
	case count > 1:
		return nil, fmt.Errorf("%d receipts match ts prefix %q — lengthen the prefix", count, tsPrefix)
	}
	var m map[string]any
	if err := json.Unmarshal(lines[matched], &m); err != nil {
		return nil, err
	}
	m["quality"] = verdict
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	lines[matched] = b
	return bytes.Join(lines, []byte("\n")), nil
}
