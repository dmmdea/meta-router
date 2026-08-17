package main

// W9-P item 1 e2e: -tpl records the template in the index identity, refresh
// preserves it, and an identity the binary does not know refuses before
// touching the index.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildTplRecordsIdentityE2E(t *testing.T) {
	bin := buildMRIndex(t)
	srv := fakeEmbedder(t)
	work := t.TempDir()
	root := filepath.Join(work, "skills")
	writeSkillDir(t, root, "alpha")
	outPath := filepath.Join(work, "meta", "index.json")

	cmd := exec.Command(bin, "build", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath, "-tpl", "tpl1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build -tpl tpl1 failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "identity embeddinggemma/tpl1") {
		t.Fatalf("build output must name the identity:\n%s", out)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Model != "embeddinggemma/tpl1" {
		t.Fatalf("index identity: %q", idx.Model)
	}

	// A refresh preserves the identity (and succeeds with no flags), stamps
	// the guard, and records the identity durably in refresh.log — the log
	// must never be identity-blind (review 2026-08-16).
	cmd = exec.Command(bin, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("refresh of templated index failed: %v\n%s", err, out)
	}
	data, _ = os.ReadFile(outPath)
	var full struct {
		Model    string `json:"model"`
		TplGuard string `json:"tpl_guard"`
	}
	if err := json.Unmarshal(data, &full); err != nil {
		t.Fatal(err)
	}
	if full.Model != "embeddinggemma/tpl1" || full.TplGuard != "tpl1" {
		t.Fatalf("refresh must preserve identity+guard, got model=%q guard=%q", full.Model, full.TplGuard)
	}
	log := readRefreshLog(t, filepath.Join(work, "meta"))
	if last := log[len(log)-1]; last.Identity != "embeddinggemma/tpl1" {
		t.Fatalf("refresh.log must carry the identity, got %+v", last)
	}
}

// A refresh that finds NO index builds fresh — always raw (templated indexes
// only ever come from an explicit `build -tpl`) — and refresh.log records the
// resulting identity, so a templated deployment silently reverting to raw
// after an index loss leaves a durable trace (review 2026-08-16).
func TestRefreshFreshBuildIsRawAndLogged(t *testing.T) {
	bin := buildMRIndex(t)
	srv := fakeEmbedder(t)
	work := t.TempDir()
	root := filepath.Join(work, "skills")
	writeSkillDir(t, root, "alpha")
	outDir := filepath.Join(work, "meta")
	outPath := filepath.Join(outDir, "index.json")

	cmd := exec.Command(bin, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh refresh failed: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(outPath)
	var idx struct {
		Model    string `json:"model"`
		TplGuard string `json:"tpl_guard"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Model != "embeddinggemma" || idx.TplGuard != "" {
		t.Fatalf("fresh-from-refresh must be raw embeddinggemma, got model=%q guard=%q", idx.Model, idx.TplGuard)
	}
	log := readRefreshLog(t, outDir)
	if last := log[len(log)-1]; last.Identity != "embeddinggemma" {
		t.Fatalf("refresh.log must record the fresh identity, got %+v", last)
	}
}

func TestBuildUnknownTplRefusesE2E(t *testing.T) {
	bin := buildMRIndex(t)
	work := t.TempDir()
	root := filepath.Join(work, "skills")
	writeSkillDir(t, root, "alpha")
	outPath := filepath.Join(work, "meta", "index.json")

	cmd := exec.Command(bin, "build", "-skill-roots", root, "-endpoint", "http://127.0.0.1:1", "-out", outPath, "-tpl", "tpl9")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("build -tpl tpl9 must refuse:\n%s", out)
	}
	if !strings.Contains(string(out), "tpl9") {
		t.Fatalf("refusal must name the unknown template:\n%s", out)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatal("refused build must write no index")
	}
}

// The -tpl/-embed-model flags are build-only: on refresh they imply a
// migration that would silently not happen. The rejection is on flag
// PRESENCE — passing the default value explicitly is still an operator
// asking for a migration refresh cannot perform (review 2026-08-16).
func TestRefreshRejectsTplFlags(t *testing.T) {
	if _, err := parseArgs([]string{"refresh", "-tpl", "tpl1"}); err == nil {
		t.Fatal("refresh -tpl must be rejected")
	}
	if _, err := parseArgs([]string{"refresh", "-embed-model", "qwen3-embedding-4b-q4"}); err == nil {
		t.Fatal("refresh -embed-model must be rejected")
	}
	if _, err := parseArgs([]string{"refresh", "-embed-model", "embeddinggemma"}); err == nil {
		t.Fatal("refresh -embed-model at its default value must still be rejected (presence, not value)")
	}
	if _, err := parseArgs([]string{"build", "-tpl", "tpl1"}); err != nil {
		t.Fatalf("build -tpl must parse: %v", err)
	}
}

// A refresh against an index recording an unknown template refuses, logs the
// failure durably, and leaves the index bytes untouched.
func TestRefreshUnknownTemplateRefusesE2E(t *testing.T) {
	bin := buildMRIndex(t)
	srv := fakeEmbedder(t)
	work := t.TempDir()
	root := filepath.Join(work, "skills")
	writeSkillDir(t, root, "alpha")
	outDir := filepath.Join(work, "meta")
	outPath := filepath.Join(outDir, "index.json")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	future := `{"model":"embeddinggemma/tpl9","dim":2,"built_unix":1,"entries":[{"skill":{"id":"skills:alpha","name":"alpha"},"vec":[1,0],"hash":"h"}]}`
	if err := os.WriteFile(outPath, []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("refresh of an unknown-template index must fail:\n%s", out)
	}
	if !strings.Contains(string(out), "tpl9") {
		t.Fatalf("failure must name the template:\n%s", out)
	}
	data, _ := os.ReadFile(outPath)
	if string(data) != future {
		t.Fatal("refused refresh must leave the index bytes untouched")
	}
	log := readRefreshLog(t, outDir)
	last := log[len(log)-1]
	if last.OK || !strings.Contains(last.Error, "tpl9") {
		t.Fatalf("refusal must be durably logged: %+v", last)
	}
	if last.Identity != "embeddinggemma/tpl9" {
		t.Fatalf("the refused row must name the identity it refused on: %+v", last)
	}
}
