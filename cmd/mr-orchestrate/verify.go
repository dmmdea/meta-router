package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// runVerify is the RS8 gate entrypoint: one tiny live sonnet capture, key-set
// diffed against the committed fixture. Run after every claude version bump
// (the policy watch alerts on those). Breaking drift returns an error so the
// exit code fails CI-style wrappers.
func runVerify(fixtureDir string) error {
	fixture, err := os.ReadFile(filepath.Join(fixtureDir, "result-sonnet.json"))
	if err != nil {
		return fmt.Errorf("committed fixture missing (run `probe --model sonnet` first): %w", err)
	}
	cmd := exec.Command("claude", "-p", "Reply with exactly: ok",
		"--model", "sonnet", "--output-format", "json")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("live verify capture failed: %w", err)
	}
	report, breaking, err := verifySchema(fixture, out.Bytes())
	if err != nil {
		return err
	}
	fmt.Print(report)
	if breaking {
		return fmt.Errorf("schema gate FAILED: breaking drift — refresh fixtures and re-run the parser tests before trusting the ledger")
	}
	return nil
}
