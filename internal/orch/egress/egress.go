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
// The PREDICATE is lane-generic — ThirdPartyLanes already covers the approved
// free lanes (Groq, Cloudflare, the gated Gemini tier, NIM) so none of them
// re-decides what "third-party" means. ENFORCEMENT is not inherited, though:
// every lane adapter must call Plan itself, and an adapter that forgets is
// simply ungated. That is exactly why the B14 canary fails any run<Lane>Lane
// dispatcher for a lane in this set that does not reach the gate.
package egress

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ThirdPartyLanes is the gated set. It is EXPORTED so the B14 canary and the
// gate share one definition: seating a new free lane here without wiring its
// adapter to Plan then fails a test instead of opening a silent hole.
var ThirdPartyLanes = []string{"glm", "groq", "cloudflare", "gemini", "nim"}

// ThirdParty reports whether a lane sends data outside the operator's
// subscription providers. claude and codex are the operator's own subscription
// relationships; local never leaves the machine.
func ThirdParty(lane string) bool {
	for _, l := range ThirdPartyLanes {
		if l == lane {
			return true
		}
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

// Plan resolves the directory a third-party dispatch must actually run in and
// decides whether it may run at all. Callers MUST use the returned dir.
//
// The subtlety that makes this function necessary: an empty CWD does NOT mean
// "no repository context". os/exec runs a child in the PARENT's current
// directory when Cmd.Dir is empty, so a dispatch launched from inside a client
// checkout would hand that checkout to the lane while the gate cheerfully
// recorded "prompt-only" on the receipt (found in review, 2026-07-25 — it
// defeated the entire gate on the most common invocation).
//
// So: when no CWD was requested we resolve the INHERITED one and gate on that.
// If it is not allowlisted we do not refuse — we make prompt-only TRUE by
// running in a neutral empty directory. The promise becomes enforced instead of
// assumed. cleanup is never nil; call it when the dispatch completes.
func Plan(lane, requestedCWD string, opt Options) (dir string, cleanup func(), d Decision) {
	noop := func() {}
	if !ThirdParty(lane) {
		return requestedCWD, noop, Decision{Allowed: true, Reason: "lane is not third-party"}
	}
	effective := requestedCWD
	if effective == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", noop, Decision{Allowed: false,
				Reason: "third-party lane " + lane + " denied: the inherited working directory cannot be resolved (fail closed)"}
		}
		effective = wd
	}
	if d := Check(lane, effective, opt); d.Allowed {
		return effective, noop, d
	} else if requestedCWD != "" {
		// The operator explicitly asked for this repo's context. Refuse.
		return "", noop, d
	}
	// No repo context was requested; the inherited cwd merely happens to be
	// somewhere unallowlisted. Enforce genuine prompt-only.
	if opt.PromptOnlyDenied {
		return "", noop, Decision{Allowed: false,
			Reason: "third-party lane " + lane + " denied: prompt-only egress is closed by policy"}
	}
	tmp, err := os.MkdirTemp("", "mr-neutral-*")
	if err != nil {
		return "", noop, Decision{Allowed: false,
			Reason: "third-party lane " + lane + " denied: cannot create a neutral working directory (fail closed): " + err.Error()}
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, Decision{Allowed: true,
		Reason: "prompt-only ENFORCED in a neutral directory (the inherited cwd " + effective + " is not allowlisted, so it is not used)"}
}

// Check decides whether a dispatch may run on the given lane with an EFFECTIVE
// working directory. Prefer Plan, which resolves an inherited cwd and can
// enforce prompt-only; Check alone trusts the cwd it is handed.
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
	// Resolve links on the cwd side: filepath.Abs is LEXICAL, so a junction
	// inside an allowlisted repo would otherwise re-export its target.
	// Fail closed if the path cannot be resolved at all.
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = real
	} else if !os.IsNotExist(rerr) {
		return Decision{Allowed: false,
			Reason: fmt.Sprintf("third-party lane %s denied: %s cannot be resolved through links (fail closed)", lane, abs)}
	}
	for _, root := range opt.AllowRepos {
		if root == "" {
			continue
		}
		rabs, rerr := filepath.Abs(root)
		if rerr != nil {
			continue // an unresolvable allowlist entry allows nothing
		}
		if real, lerr := filepath.EvalSymlinks(rabs); lerr == nil {
			rabs = real
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
	// filepath.Rel already folds case per-element on Windows and errors across
	// volumes; this anchor re-check is belt-and-braces (verified: Rel("D:/Dev",
	// "d:/dev/repo") = "repo").
	if !strings.EqualFold(filepath.VolumeName(p), filepath.VolumeName(root)) {
		return false
	}
	return true
}

// AddDirs extracts directories a lane's --extra arguments would grant the child
// beyond its working directory. Claude Code's --add-dir widens file access, so
// gating only cwd would leave a second, unmeasured export channel open (found
// in review, 2026-07-25). Accepts both "--add-dir X" and "--add-dir=X".
func AddDirs(extra []string) []string {
	var out []string
	for i := 0; i < len(extra); i++ {
		a := extra[i]
		switch {
		case a == "--add-dir":
			if i+1 < len(extra) {
				out = append(out, extra[i+1])
				i++
			}
		case strings.HasPrefix(a, "--add-dir="):
			out = append(out, strings.TrimPrefix(a, "--add-dir="))
		}
	}
	return out
}
