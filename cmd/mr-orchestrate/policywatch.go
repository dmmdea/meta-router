package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RS7 policy watch — MULTI-VENDOR tripwire (all four lanes have policy
// surfaces that have moved on <24h notice):
//   - Anthropic: support article 15036540 (subscription/API billing split is
//     PAUSED, not dead) + claude CLI version (schema-drift trigger, RS8).
//   - z.ai/GLM: the coding-plan usage policy page (fair-usage enforcement
//     changed abruptly in April 2026; 1313 hard-stop class) — watchable
//     without credentials.
//   - OpenAI/Codex: codex CLI version (vendors ship breaking JSONL renames
//     unversioned — codex #4776).
//
// Fail-open: a fetch failure is a note, never a crash — and it must NOT wipe
// the stored baseline, or the very change the watch exists to catch would
// arrive unalerted after an outage. Alerts LATCH (alert_since) until
// `probe --policy --ack`, because an unattended nightly that alerts and then
// self-clears the next night was never seen.

const (
	policyArticleURL = "https://support.claude.com/en/articles/15036540"
	zaiPolicyURL     = "https://docs.z.ai/devpack/usage-policy"
)

type policyState struct {
	CheckedAt         time.Time  `json:"checked_at"`
	CLIVersion        string     `json:"cli_version"`
	LastCLIVersion    string     `json:"last_cli_version,omitempty"`
	CodexVersion      string     `json:"codex_version,omitempty"`
	LastCodexVersion  string     `json:"last_codex_version,omitempty"`
	ArticleHash       string     `json:"article_hash,omitempty"`
	LastArticleHash   string     `json:"last_article_hash,omitempty"`
	ZaiPolicyHash     string     `json:"zai_policy_hash,omitempty"`
	LastZaiPolicyHash string     `json:"last_zai_policy_hash,omitempty"`
	Alert             bool       `json:"alert"`
	AlertSince        *time.Time `json:"alert_since,omitempty"`
	Notes             []string   `json:"notes"`
}

// observed is one probe cycle's raw readings ("" = unavailable this cycle).
type observed struct {
	ClaudeVersion string
	CodexVersion  string
	ArticleHash   string
	ZaiHash       string
	FetchNotes    []string
}

var (
	scriptBlocks = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tags         = regexp.MustCompile(`(?s)<[^>]*>`)
	spaces       = regexp.MustCompile(`\s+`)
	// Intercom/doc sites render relative-time badges ("Updated over 2 weeks
	// ago") in the body — they drift with time, not policy. Strip pre-hash.
	updatedBadge = regexp.MustCompile(`(?i)updated\s+[\w\s]{0,30}?\bago\b`)
)

// stripHTMLText reduces a page to its visible text so per-request script
// nonces, asset hashes, and relative-time badges don't fire false alerts.
func stripHTMLText(html []byte) string {
	s := scriptBlocks.ReplaceAllString(string(html), " ")
	s = tags.ReplaceAllString(s, " ")
	s = updatedBadge.ReplaceAllString(s, " ")
	s = spaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len(s) > 4000 {
		s = s[:4000]
	}
	return s
}

func hashText(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func cliVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}

func fetchPage(url string) ([]byte, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}

// evalPolicy is the pure comparison core (unit-tested); prev may be zero.
// Baselines survive outages; alerts latch until ack.
func evalPolicy(prev policyState, obs observed, now time.Time) policyState {
	st := policyState{
		CheckedAt:  now,
		CLIVersion: obs.ClaudeVersion, LastCLIVersion: prev.CLIVersion,
		CodexVersion: obs.CodexVersion, LastCodexVersion: prev.CodexVersion,
		ArticleHash: obs.ArticleHash, LastArticleHash: prev.ArticleHash,
		ZaiPolicyHash: obs.ZaiHash, LastZaiPolicyHash: prev.ZaiPolicyHash,
	}
	st.Notes = append(st.Notes, obs.FetchNotes...)
	if obs.ArticleHash == "" {
		st.ArticleHash = prev.ArticleHash // never wipe a baseline on an outage
	}
	if obs.ZaiHash == "" {
		st.ZaiPolicyHash = prev.ZaiPolicyHash
	}
	alertNow := func(note string) {
		st.Alert = true
		if st.AlertSince == nil {
			t := now
			st.AlertSince = &t
		}
		st.Notes = append(st.Notes, note)
	}
	if prev.Alert { // latch until `probe --policy --ack`
		st.Alert = true
		st.AlertSince = prev.AlertSince
		st.Notes = append(st.Notes, "alert LATCHED (ack with `mr-orchestrate probe --policy --ack`)")
	}
	check := func(kind, prevV, curV, note string) {
		switch {
		case prevV == "":
			if curV != "" {
				st.Notes = append(st.Notes, "baseline seeded: "+kind)
			}
		case curV != "" && prevV != curV:
			alertNow(note)
		}
	}
	check("claude cli version", prev.CLIVersion, obs.ClaudeVersion,
		fmt.Sprintf("claude CLI changed %s -> %s: schema gate auto-run follows; re-smoke GLM when the lane is live (2.1.196 feature-gated non-Anthropic base URLs)", prev.CLIVersion, obs.ClaudeVersion))
	check("codex cli version", prev.CodexVersion, obs.CodexVersion,
		fmt.Sprintf("codex CLI changed %s -> %s: re-verify exec --json event schema against testdata/fixtures/codex (vendors rename fields unversioned, #4776)", prev.CodexVersion, obs.CodexVersion))
	check("support article 15036540", prev.ArticleHash, obs.ArticleHash,
		"Anthropic support article 15036540 CHANGED — check whether the subscription/API billing split resumed (RS7)")
	check("z.ai usage policy", prev.ZaiPolicyHash, obs.ZaiHash,
		"z.ai coding-plan usage policy CHANGED — re-read fair-usage/ban terms before further GLM-lane traffic (1313 class)")
	return st
}

func runPolicyWatch(ack bool) error {
	now := time.Now().UTC()
	var prev policyState
	if raw, err := os.ReadFile(policyAlertPath()); err == nil {
		_ = json.Unmarshal(raw, &prev)
	}
	if ack {
		prev.Alert = false
		prev.AlertSince = nil
	}
	obs := observed{ClaudeVersion: cliVersion("claude"), CodexVersion: cliVersion("codex")}
	if body, err := fetchPage(policyArticleURL); err != nil {
		obs.FetchNotes = append(obs.FetchNotes, "anthropic article fetch failed (fail-open, baseline preserved): "+err.Error())
	} else {
		obs.ArticleHash = hashText(stripHTMLText(body))
	}
	if body, err := fetchPage(zaiPolicyURL); err != nil {
		obs.FetchNotes = append(obs.FetchNotes, "z.ai policy fetch failed (fail-open, baseline preserved): "+err.Error())
	} else {
		obs.ZaiHash = hashText(stripHTMLText(body))
	}
	st := evalPolicy(prev, obs, now)
	// RS8 wire-in: a claude CLI version change immediately exercises the
	// schema gate (one tiny live call) instead of leaving an advisory note.
	if prev.CLIVersion != "" && obs.ClaudeVersion != "" && prev.CLIVersion != obs.ClaudeVersion {
		if err := runVerify(fixturesDir()); err != nil {
			st.Notes = append(st.Notes, "schema gate: "+err.Error())
		} else {
			st.Notes = append(st.Notes, "schema gate auto-run: stable")
		}
	}
	// RS8 codex leg (Task 4): a codex CLI version change re-verifies the
	// exec --json event schema the same way (vendors rename fields
	// unversioned, codex #4776).
	if prev.CodexVersion != "" && obs.CodexVersion != "" && prev.CodexVersion != obs.CodexVersion {
		if err := runVerifyCodex(codexFixturesDir()); err != nil {
			st.Notes = append(st.Notes, "codex schema gate: "+err.Error())
		} else {
			st.Notes = append(st.Notes, "codex schema gate auto-run: stable")
		}
	}
	if err := os.MkdirAll(filepath.Dir(policyAlertPath()), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(policyAlertPath(), out, 0o644); err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
