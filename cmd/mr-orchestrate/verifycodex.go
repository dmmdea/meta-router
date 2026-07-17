package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/codexlane"
)

// RS8 codex leg: the exec --json stream is JSONL, so the schema gate is the
// per-event-type variant of verifySchema — every key path is prefixed by its
// event type (turn.completed.usage.input_tokens). Removed keys are breaking;
// added keys are advisory (vendors rename fields unversioned, codex #4776).

// jsonlKeys walks each JSONL line emitting event-type-prefixed key paths.
func jsonlKeys(jsonl []byte) (map[string]bool, error) {
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(jsonl))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, fmt.Errorf("jsonl line unparseable: %w (%s)", err, line)
		}
		typ, _ := v["type"].(string)
		if typ == "" {
			return nil, fmt.Errorf("jsonl event without a type: %s", line)
		}
		keyPaths(v, typ, out)
	}
	return out, sc.Err()
}

// verifyCodexSchema compares a live JSONL capture against the committed
// fixture, per event type. Same contract as verifySchema: removed = breaking.
func verifyCodexSchema(fixtureRaw, liveRaw []byte) (report string, breaking bool, err error) {
	fk, err := jsonlKeys(fixtureRaw)
	if err != nil {
		return "", false, fmt.Errorf("fixture unparseable: %w", err)
	}
	lk, err := jsonlKeys(liveRaw)
	if err != nil {
		return "", false, fmt.Errorf("live capture unparseable: %w", err)
	}
	removed, added := diffKeys(fk, lk)
	var b bytes.Buffer
	fmt.Fprintf(&b, "codex schema gate: fixture=%d keys, live=%d keys\n", len(fk), len(lk))
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

// runVerifyCodex is the RS8 codex gate entrypoint: ONE tiny live exec --json
// turn (AUTHORIZED, low effort — the Plus window is degraded), key-set diffed
// per event type against the committed fixture. Auto-run by the policy watch
// on codex CLI version changes.
func runVerifyCodex(fixtureDir string) error {
	fixture, err := os.ReadFile(filepath.Join(fixtureDir, "exec-events.jsonl"))
	if err != nil {
		return fmt.Errorf("committed codex fixture missing (capture one with `probe --codex` first): %w", err)
	}
	home, cleanup, err := codexlane.EnsureHome(stateDir())
	if err != nil {
		return err
	}
	defer cleanup()
	o, raw, err := codexlane.Run(context.Background(), codexlane.RunReq{
		Prompt: "Reply with exactly: ok", Model: "gpt-5.5", Effort: "low",
		Home: home, TimeoutSec: 300,
	})
	if err != nil {
		return fmt.Errorf("live codex verify capture failed: %w", err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("live codex verify produced no stream (outcome %s: %s)", o.Class, o.Result)
	}
	report, breaking, err := verifyCodexSchema(fixture, raw)
	if err != nil {
		return err
	}
	fmt.Print(report)
	if breaking {
		return fmt.Errorf("codex schema gate FAILED: breaking drift — refresh testdata/fixtures/codex and re-run the parser tests before trusting the ledger")
	}
	return nil
}

// writeCodexAlert latches the burn-anomaly alert (slice-1 policy/1313 shape):
// the FIRST violation's note+timestamp is the evidence — later anomalies never
// overwrite it. `probe --ack-codex` clears. The latch does NOT auto-mutate
// capacity; it makes the misfire visible.
func writeCodexAlert(path, note string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(map[string]any{"note": note, "since": now}, "", "  ")
	if err != nil {
		return err
	}
	// Atomic create-once (A2R-#7): mirror LatchAlert — O_CREATE|O_EXCL so
	// concurrent racers cannot clobber the first violation's timestamp.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil // already latched — first anomaly stands
		}
		return err
	}
	defer f.Close()
	_, werr := f.Write(b)
	return werr
}
