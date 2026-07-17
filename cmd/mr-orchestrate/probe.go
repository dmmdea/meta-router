package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/codexlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
)

// ingestStreamSignal feeds a stream-json capture's rate_limit_events into the
// ledger (Task 8): a probe --stream capture is real provider signal — use it,
// don't just file it. Writes go through the cross-process Update transaction;
// the S2R-7 fail-open semantics (known-status sets, staleness guard,
// authoritative-overwrite anchor) live in quotasig.IngestStreamEvents.
func ingestStreamSignal(raw []byte, now time.Time) (int, error) {
	n := 0
	err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		n = quotasig.IngestStreamEvents(l, raw, "claude", now)
	})
	return n, err
}

// runProbe captures sanitized live fixtures. The operator authorized probes 2026-07-05
// ("you can probe all you want"). Tiny prompts only; every run pins --model (hard rule).
func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	model := fs.String("model", "sonnet", "model to pin for the probe")
	stream := fs.Bool("stream", false, "capture stream-json events instead of result json")
	outDir := fs.String("out", fixturesDir(), "fixture output dir (MR_ORCH_FIXTURES overrides the default)")
	verify := fs.Bool("verify", false, "RS8 schema gate: re-capture live and diff key-set vs the committed fixture")
	policy := fs.Bool("policy", false, "RS7 policy watch: support-article + CLI-version check, writes the alert file")
	ack := fs.Bool("ack", false, "with --policy: acknowledge and clear a latched alert, re-seeding baselines")
	codexCap := fs.Bool("codex", false, "capture a sanitized codex exec --json fixture (tiny, authorized)")
	verifyCodex := fs.Bool("verify-codex", false, "RS8 codex leg: one tiny live exec --json, per-event-type key diff vs the committed fixture")
	ackCodex := fs.Bool("ack-codex", false, "acknowledge and clear a latched codex burn-anomaly alert")
	ackGLM := fs.Bool("ack-glm", false, "acknowledge and clear a latched GLM 1313 hard-stop (the R11 two-step: ack, then re-run)")
	codexUsage := fs.Bool("codex-usage", false, "capture the wham/usage endpoint ONCE for schema inspection (requires codex_usage_poll=true — D3-class, ships OFF)")
	_ = fs.Parse(args)

	if *policy {
		return runPolicyWatch(*ack)
	}
	if *verify {
		return runVerify(*outDir)
	}
	if *verifyCodex {
		return runVerifyCodex(codexFixturesDir())
	}
	if *ackCodex {
		if err := os.Remove(codexAlertPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("codex burn-anomaly alert cleared")
		return nil
	}
	if *ackGLM {
		if err := os.Remove(glmAlertPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("glm 1313 hard-stop latch cleared — the account has been warned; dispatch accordingly")
		return nil
	}
	if *codexUsage {
		return runCodexUsageCapture()
	}
	if *codexCap {
		dir := *outDir
		if dir == fixturesDir() { // default untouched → the codex fixture home
			dir = codexFixturesDir()
		}
		return runCodexCapture(dir)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	format := "json"
	name := "result-" + *model + ".json"
	cmdArgs := []string{"-p", "Reply with exactly: ok", "--model", *model, "--output-format", format}
	if *stream {
		format = "stream-json"
		name = "stream-events-" + *model + ".jsonl"
		// stream-json in print mode requires --verbose (CLI 2.1.x)
		cmdArgs = []string{"-p", "Reply with exactly: ok", "--model", *model, "--output-format", format, "--verbose"}
	}
	cmd := exec.Command("claude", cmdArgs...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		// A refusal/error capture is still a fixture — write whatever arrived, then report.
		if out.Len() > 0 {
			dst := filepath.Join(*outDir, name)
			_ = os.WriteFile(dst, sanitize(out.Bytes()), 0o644)
			if *stream { // an errored capture can still carry the rate_limit_event
				ingestStreamCapture(out.Bytes())
			}
			fmt.Printf("probe: run errored but wrote %s (%d bytes) — inspect; the outcome IS the fixture\n", dst, out.Len())
		}
		return fmt.Errorf("probe run (%s): %w", *model, err)
	}
	clean := sanitize(out.Bytes())
	dst := filepath.Join(*outDir, name)
	if err := os.WriteFile(dst, clean, 0o644); err != nil {
		return err
	}
	if *stream {
		ingestStreamCapture(clean)
	}
	fmt.Printf("probe: wrote %s (%d bytes, %.1fs)\n", dst, len(clean), time.Since(start).Seconds())
	return nil
}

// ingestStreamCapture is the probe-side wrapper: ingest, WARN on failure
// (fail-open — the fixture on disk is never lost), note applied events.
func ingestStreamCapture(raw []byte) {
	n, err := ingestStreamSignal(raw, time.Now().UTC())
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "warn: stream ingest failed (capture still on disk):", err)
	case n > 0:
		fmt.Fprintf(os.Stderr, "stream ingest: %d rate_limit_event(s) applied to the ledger\n", n)
	}
}

// runCodexCapture writes a sanitized codex exec --json fixture (the codex
// analogue of the claude probe: tiny prompt, low effort, model pinned).
func runCodexCapture(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	home, cleanup, err := codexlane.EnsureHome(stateDir())
	if err != nil {
		return err
	}
	defer cleanup()
	start := time.Now()
	o, raw, err := codexlane.Run(context.Background(), codexlane.RunReq{
		Prompt: "Reply with exactly: ok", Model: "gpt-5.5", Effort: "low",
		Home: home, TimeoutSec: 300,
	})
	if err != nil {
		return fmt.Errorf("probe run (codex): %w", err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("codex probe produced no stream (outcome %s: %s)", o.Class, o.Result)
	}
	dst := filepath.Join(dir, "exec-events.jsonl")
	if err := os.WriteFile(dst, sanitize(raw), 0o644); err != nil {
		return err
	}
	fmt.Printf("probe: wrote %s (%d bytes, %.1fs, outcome %s)\n", dst, len(raw), time.Since(start).Seconds(), o.Class)
	return nil
}

// runCodexUsageCapture is the config-gated usage-poll SLOT (Task 4 design
// decision, recorded): the only usage endpoint is chatgpt.com/backend-api/
// wham/usage (CodeyBox prior art) — reading it reuses the ChatGPT OAuth
// token, the same ToS-gray class as the Claude oauth/usage poll the operator kept
// OFF (D3). S2R-16: ecosystem evidence (codex CLI itself polls it ~60s;
// CodexBar et al. read it broadly, zero found ban reports) is recorded for a
// future operator re-call; until that explicit flip, the gate stays OFF and
// the codex ledger is telemetry-learned. The parser is written only AFTER a
// real capture exists (fixtures-first; no code against an unseen schema).
func runCodexUsageCapture() error {
	cfg := orchcfg.Load(configPath())
	if !cfg.CodexUsagePoll {
		return fmt.Errorf("codex_usage_poll is OFF (D3-class risk call, S2R-16): flipping it in config.json is the operator's explicit decision — until then the codex ledger is telemetry-learned")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return fmt.Errorf("codex auth missing: run `codex login` (R12): %w", err)
	}
	tok := findStringField(raw, "access_token")
	if tok == "" {
		return fmt.Errorf("no access_token in ~/.codex/auth.json (schema drift?) — value intentionally not logged (R10)")
	}
	req, err := http.NewRequest("GET", "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("wham/usage: http %d (body withheld from logs)", resp.StatusCode)
	}
	dst := filepath.Join(codexFixturesDir(), "usage-endpoint.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		return err
	}
	fmt.Printf("probe: wrote %s (%d bytes) — inspect the schema BEFORE writing any parser (fixtures-first)\n", dst, len(body))
	return nil
}

// findStringField walks arbitrary JSON for the first string value under key.
// Tolerant by design: the auth.json layout is vendor-owned and undocumented.
func findStringField(raw []byte, key string) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(n any) string {
		switch t := n.(type) {
		case map[string]any:
			if s, ok := t[key].(string); ok && s != "" {
				return s
			}
			for _, c := range t {
				if s := walk(c); s != "" {
					return s
				}
			}
		case []any:
			for _, c := range t {
				if s := walk(c); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(v)
}

var idFields = regexp.MustCompile(`"(session_id|uuid|leafUuid|thread_id)"\s*:\s*"[^"]*"`)

// sanitize zeroes identifiers so fixtures carry no session linkage.
func sanitize(b []byte) []byte {
	out := idFields.ReplaceAll(b, []byte(`"$1":"SANITIZED"`))
	// pretty-print single-object results for reviewable fixtures; leave JSONL alone
	var v any
	if json.Unmarshal(out, &v) == nil {
		if p, err := json.MarshalIndent(v, "", "  "); err == nil {
			return p
		}
	}
	return out
}
