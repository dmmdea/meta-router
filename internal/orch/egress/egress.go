// Package egress is the data-boundary gate: it decides whether a dispatch may
// send this operator's prompts and repository context to a THIRD-PARTY lane —
// one whose provider is neither Anthropic nor OpenAI and whose data handling is
// outside the subscription relationships the rest of the system assumes.
//
// Why it exists (audit 2026-07-25): GLM is rank 1 for the default coding
// classes, runs with cmd.Dir set to a real repository, and ships prompt plus
// repo context to a PRC-hosted provider — with no gate of any kind. The
// operator's own recorded rule for the Gemini free tier was that it "is not
// seated until the non-sensitive-prompt allowlist gate exists in the adapter …
// Multi-brand isolation makes this gate non-negotiable." That rule was written
// for a lane that had not shipped while a live lane had been leaking the whole
// time. One misrouted workhorse-coding dispatch inside a client checkout ships
// that client's code.
//
// Design:
//   - DENY BY DEFAULT for repository context. A dispatch that carries a working
//     directory may only run on a third-party lane when that repo is on the
//     operator's explicit allowlist.
//   - Prompt-only dispatches (no CWD) are allowed by default: nothing but the
//     prompt the caller wrote leaves, and gating those would break every
//     non-repo use with no data-boundary gain. Set PromptOnlyDenied to close it.
//   - FORCE-PROOF. Like the R10 billing hard-stop, --force does not bypass this:
//     --force exists to override QUOTA judgement, never to export data.
//   - Fail CLOSED on anything unrecognized: an unreadable path, a relative path
//     that cannot be resolved, or an empty allowlist all deny.
//
// The gate is lane-generic so the approved free lanes (Groq, Cloudflare, and
// the gated Gemini tier) inherit it rather than each re-deciding.
package egress

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ThirdParty reports whether a lane sends data outside the operator's
// subscription providers. claude and codex are the operator's own subscription
// relationships; local never leaves the machine.
func ThirdParty(lane string) bool {
	switch lane {
	case "glm", "groq", "cloudflare", "gemini", "nim":
		return true
	}
	return false
}

// Decision is the gate's verdict, recorded on the dispatch receipt so an
// export is always countable after the fact.
type Decision struct {
	Allowed bool
	// Reason is always populated: on a denial it explains what to do, and on an
	// allow it names WHY (which allowlist entry matched, or prompt-only), so a
	// receipt can never leave the basis of an export implicit.
	Reason string
}

// Options are the operator's egress policy for third-party lanes.
type Options struct {
	// AllowRepos are absolute paths whose subtrees may be sent to a third-party
	// lane. A dispatch's CWD must be inside one of them.
	AllowRepos []string
	// PromptOnlyDenied closes the no-CWD case too (maximum strictness).
	PromptOnlyDenied bool
}

// Check decides whether a dispatch may run on the given lane.
// cwd is the working directory the lane adapter would set ("" = prompt-only).
func Check(lane, cwd string, opt Options) Decision {
	if !ThirdParty(lane) {
		return Decision{Allowed: true, Reason: "lane is not third-party"}
	}
	if cwd == "" {
		if opt.PromptOnlyDenied {
			return Decision{Allowed: false,
				Reason: "third-party lane " + lane + " denied: prompt-only egress is closed by policy"}
		}
		return Decision{Allowed: true, Reason: "prompt-only (no repository context leaves)"}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return Decision{Allowed: false,
			Reason: fmt.Sprintf("third-party lane %s denied: working directory %q cannot be resolved (fail closed)", lane, cwd)}
	}
	if len(opt.AllowRepos) == 0 {
		return Decision{Allowed: false,
			Reason: fmt.Sprintf("third-party lane %s denied: dispatch carries repository context (%s) and no repo is allowlisted. Add the repo to glm_allow_repos in config.json, or dispatch without a working directory.", lane, abs)}
	}
	for _, root := range opt.AllowRepos {
		if root == "" {
			continue
		}
		rabs, rerr := filepath.Abs(root)
		if rerr != nil {
			continue // an unresolvable allowlist entry allows nothing
		}
		if within(abs, rabs) {
			return Decision{Allowed: true,
				Reason: "repository context allowlisted: " + rabs}
		}
	}
	return Decision{Allowed: false,
		Reason: fmt.Sprintf("third-party lane %s denied: %s is not inside any allowlisted repo (fail closed — multi-brand isolation)", lane, abs)}
}

// within reports whether path p is inside root, comparing cleaned paths and
// treating Windows case-insensitively. A prefix test alone would let
// "C:/repos/client-secret" pass for the root "C:/repos/client".
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return false
	}
	// filepath.Rel is case-sensitive on Windows; re-check the anchor.
	if !strings.EqualFold(filepath.VolumeName(p), filepath.VolumeName(root)) {
		return false
	}
	return true
}
