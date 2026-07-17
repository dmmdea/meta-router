package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RS8 continuous schema gate: both vendors have shipped breaking JSON renames
// without versioning (codex #4776) and silent format bugs (#15451, #25670).
// probe --verify re-captures a tiny live result and diffs its KEY-SET against
// the committed fixture, so drift is caught before the parser mis-attributes.

// keyPaths walks a JSON value emitting dotted key paths. Arrays contribute
// their first element only; the per-model keys under modelUsage are
// normalized to "*" (the model name is data, not schema).
func keyPaths(v any, prefix string, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			name := k
			if strings.HasSuffix(prefix, "modelUsage") {
				name = "*"
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			out[p] = true
			keyPaths(child, p, out)
		}
	case []any:
		if len(t) > 0 {
			keyPaths(t[0], prefix, out)
		}
	}
}

func jsonKeys(raw []byte) (map[string]bool, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	keyPaths(v, "", out)
	return out, nil
}

// diffKeys returns fixture-only (removed) and live-only (added) paths, sorted.
func diffKeys(fixture, live map[string]bool) (removed, added []string) {
	for k := range fixture {
		if !live[k] {
			removed = append(removed, k)
		}
	}
	for k := range live {
		if !fixture[k] {
			added = append(added, k)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	return
}

// verifySchema compares a live capture against the committed fixture bytes.
// Additive drift (new keys) is a warning; REMOVED keys are the breaking class
// and fail the gate.
func verifySchema(fixtureRaw, liveRaw []byte) (report string, breaking bool, err error) {
	fk, err := jsonKeys(fixtureRaw)
	if err != nil {
		return "", false, fmt.Errorf("fixture unparseable: %w", err)
	}
	lk, err := jsonKeys(liveRaw)
	if err != nil {
		return "", false, fmt.Errorf("live capture unparseable: %w", err)
	}
	removed, added := diffKeys(fk, lk)
	var b strings.Builder
	fmt.Fprintf(&b, "schema gate: fixture=%d keys, live=%d keys\n", len(fk), len(lk))
	if len(removed) == 0 && len(added) == 0 {
		b.WriteString("schema stable: no drift\n")
		return b.String(), false, nil
	}
	for _, k := range removed {
		fmt.Fprintf(&b, "REMOVED (breaking): %s\n", k)
	}
	for _, k := range added {
		fmt.Fprintf(&b, "added (advisory): %s\n", k)
	}
	return b.String(), len(removed) > 0, nil
}
