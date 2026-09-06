package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/childenv"
	"github.com/dmmdea/meta-router/internal/orch/copilotlane"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

// copilot-review asks GitHub Copilot to review a pull request — through the
// SAME admission gate every copilot dispatch passes. A Copilot code review is
// a Copilot interaction billed in AI credits (GitHub: lite $0.05–$1, balanced
// $0.25–$5 per review) plus Actions minutes, so on an exhausted month the
// request would sit unserved or fail; the gate skips it instead, loudly and
// typed, and a receipt records the skip. Skipping is a SUCCESSFUL outcome for
// the caller (exit 0 with `skipped: true`): a ship flow must not fail because
// the optional reviewer is out of budget.
//
// The request is then VERIFIED, not trusted: live on the exhausted account
// (2026-09-06) `gh pr edit --add-reviewer @copilot` exited 0 and GitHub
// recorded nothing — no reviewer, no review-requested event — so gh's exit
// code alone would have metered a review that never existed. Only a Copilot
// bot present in the PR's reviewRequests counts as requested.
//
// Account separation is enforced here because the request is a gh WRITE the
// PreToolUse guard never sees (it runs inside this binary): the repository
// owner must be the subscription account (copilot_token_user), and gh runs
// with ONLY that account's minted token — the mutable "active account" and
// any ambient GH_TOKEN are scrubbed out of the decision entirely.

// runGH executes one gh command authenticated solely by the minted token.
// A var so tests inject a recorder instead of spawning gh.
var runGH = func(token string, args ...string) (string, error) {
	c := exec.Command("gh", args...)
	c.Env = append(childenv.Scrub(os.Environ()), "GH_TOKEN="+token)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// mintReviewToken is copilotlane.MintToken behind a var for the same reason.
var mintReviewToken = copilotlane.MintToken

// reviewResult is the command's one JSON document: what happened and why.
type reviewResult struct {
	Repo           string     `json:"repo"`
	PR             int        `json:"pr"`
	Requested      bool       `json:"requested"` // a Copilot reviewer is RECORDED on the PR
	Skipped        bool       `json:"skipped"`   // the lane's gate refused; nothing was sent
	AdmitState     string     `json:"admit_state"`
	Reason         string     `json:"reason,omitempty"`
	ResumeAt       *time.Time `json:"resume_at,omitempty"`
	Forced         bool       `json:"forced,omitempty"`
	Reviewer       string     `json:"reviewer,omitempty"` // the bot login GitHub recorded
	CreditsMetered int64      `json:"credits_metered,omitempty"`
}

func runCopilotReview(args []string) error {
	fs := flag.NewFlagSet("copilot-review", flag.ContinueOnError)
	repo := fs.String("repo", "", "owner/name of the pull request's repository (required; the owner must be copilot_token_user)")
	pr := fs.Int("pr", 0, "pull request number (required)")
	force := fs.Bool("force", false, "R11: request past a throttled/exhausted lane (loud)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" || *pr <= 0 {
		return fmt.Errorf("usage: mr-orchestrate copilot-review --repo owner/name --pr N [--force]")
	}
	return copilotReview(os.Stdout, *repo, *pr, *force, time.Now().UTC())
}

func copilotReview(out io.Writer, repo string, pr int, force bool, now time.Time) error {
	owner, name, ok := strings.Cut(repo, "/")
	owner = strings.ToLower(strings.TrimSpace(owner))
	if !ok || owner == "" || strings.TrimSpace(name) == "" {
		return fmt.Errorf("--repo must be owner/name, got %q", repo)
	}
	cfg := orchcfg.Load(configPath())
	if cfg.CopilotTokenUser == "" {
		return fmt.Errorf("copilot_token_user is empty: set it in the orchestrator config to the GitHub account that owns the Copilot subscription")
	}
	if owner != strings.ToLower(cfg.CopilotTokenUser) {
		return fmt.Errorf("refusing: %s is owned by %q but the Copilot subscription account is %q — a review request on another account's repository is a cross-account gh write (account-separation rule); this binary never falls through to the active gh account", repo, owner, cfg.CopilotTokenUser)
	}
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	g := laneGate(l.Snapshot(), "copilot", now, defaultThresholds, force)
	res := reviewResult{Repo: repo, PR: pr, AdmitState: g.State, Reason: g.Reason, Forced: g.Forced}
	desc := fmt.Sprintf("%s#%d", repo, pr)
	if !g.Admit {
		res.Skipped = true
		if !g.ResumeAt.IsZero() {
			t := g.ResumeAt
			res.ResumeAt = &t
		}
		warnIf(dispatch.Append(dispatchPath(), dispatch.Record{
			TS: now, Lane: "copilot", Model: "code-review", OutcomeClass: "deferred",
			Admit: false, AdmitState: g.State, AdmitReason: g.Reason, Origin: "copilot_review", Desc: desc,
		}), "dispatch append (review skipped)")
		return emitReview(out, res)
	}
	if g.Forced {
		fmt.Fprintln(os.Stderr, "WARN:", g.Reason)
	}
	tok, err := mintReviewToken(cfg.CopilotTokenUser)
	if err != nil {
		return fmt.Errorf("copilot-review: %w", err)
	}
	ghOut, ghErr := runGH(tok, "pr", "edit", strconv.Itoa(pr), "--repo", repo, "--add-reviewer", "@copilot")
	class := "ok"
	var metered int64
	switch {
	case ghErr != nil:
		class = "error"
	default:
		// Trust nothing gh's exit code implies: ask GitHub what it recorded.
		bot, verr := copilotReviewerOn(tok, owner, strings.TrimSpace(name), pr)
		switch {
		case verr != nil:
			class = "verify_error"
			res.Reason = "gh accepted the request but the reviewer state could not be read: " + verr.Error()
		case bot == "":
			class = "not_recorded"
			res.Reason = "gh accepted the request but GitHub recorded no Copilot reviewer — Copilot code review is unavailable for this account right now (AI credits exhausted, or code review disabled under Copilot features); nothing was metered"
		default:
			// The review's credits are unknown until the vendor bills them; the
			// configured estimate keeps the month honest between polls, and the
			// next poll replaces the estimate with the vendor's figure.
			metered = cfg.CopilotReviewCredits
			res.Requested, res.Reviewer, res.CreditsMetered = true, bot, metered
			warnIf(updateLedger(func(fresh *ledger.Ledger) {
				if b, ok := fresh.Bucket("copilot", ledger.WinMonth); !ok || b.CapTokens == 0 {
					fresh.SetCapacityEstimate("copilot", ledger.WinMonth, cfg.CopilotMonthlyCredits*1000)
				}
				fresh.AnchorIfUnset("copilot", ledger.WinMonth, ledger.NextMonthlyReset(now), now)
				fresh.AddShadow("copilot", ledger.WinMonth, metered*1000, now)
			}), "ledger update (review)")
		}
	}
	warnIf(dispatch.Append(dispatchPath(), dispatch.Record{
		TS: now, Lane: "copilot", Model: "code-review", OutcomeClass: class,
		Admit: true, AdmitState: g.State, AdmitReason: g.Reason, Origin: "copilot_review", Desc: desc,
		PremiumRequests: metered,
	}), "dispatch append (review)")
	if err := emitReview(out, res); err != nil {
		return err
	}
	if class == "error" {
		return fmt.Errorf("gh pr edit --add-reviewer @copilot failed for %s: %s", desc, ghOut)
	}
	return nil
}

// copilotReviewerOn reads the PR's recorded review requests and returns the
// login of the Copilot bot among them ("" when none). gh's own JSON and the
// REST requested_reviewers list render only users and teams, so the GraphQL
// Bot fragment is the one view in which a Copilot request is visible.
func copilotReviewerOn(token, owner, name string, pr int) (string, error) {
	const q = `query($owner:String!,$name:String!,$number:Int!){ repository(owner:$owner,name:$name){ pullRequest(number:$number){ reviewRequests(first:50){ nodes{ requestedReviewer{ __typename ... on Bot{ login } ... on User{ login } } } } } } }`
	raw, err := runGH(token, "api", "graphql", "-f", "query="+q, "-F", "owner="+owner, "-F", "name="+name, "-F", "number="+strconv.Itoa(pr))
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, raw)
	}
	var doc struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewRequests struct {
						Nodes []struct {
							RequestedReviewer struct {
								Typename string `json:"__typename"`
								Login    string `json:"login"`
							} `json:"requestedReviewer"`
						} `json:"nodes"`
					} `json:"reviewRequests"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "", fmt.Errorf("undecodable reviewRequests response: %w", err)
	}
	for _, n := range doc.Data.Repository.PullRequest.ReviewRequests.Nodes {
		r := n.RequestedReviewer
		if r.Typename == "Bot" && strings.Contains(strings.ToLower(r.Login), "copilot") {
			return r.Login, nil
		}
	}
	return "", nil
}

func emitReview(out io.Writer, r reviewResult) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(b))
	return err
}
