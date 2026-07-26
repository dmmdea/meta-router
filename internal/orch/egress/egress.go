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
	abs, err := canonical(cwd)
	if err != nil {
		return Decision{Allowed: false,
			Reason: fmt.Sprintf("third-party lane %s denied: working directory %q cannot be resolved to a real path (fail closed): %v", lane, cwd, err)}
	}
	if len(opt.AllowRepos) == 0 {
		return Decision{Allowed: false,
			Reason: fmt.Sprintf("third-party lane %s denied: dispatch carries repository context (%s) and no repo is allowlisted. Add the repo to glm_allow_repos in config.json, or dispatch without a working directory.", lane, abs)}
	}
	for _, root := range opt.AllowRepos {
		if root == "" {
			continue
		}
		// A RELATIVE allowlist entry resolves against whatever directory the
		// orchestrator happens to be standing in, so "." would silently
		// allowlist every checkout in turn. Refuse it instead of resolving it.
		if !fullyQualified(root) {
			return Decision{Allowed: false,
				Reason: fmt.Sprintf("third-party lane %s denied: glm_allow_repos entry %q is not fully qualified, so what it allows depends on the orchestrator's current directory or drive (%q self-allowlists any checkout). Use an absolute path.", lane, root, ".")}
		}
		rabs, rerr := canonical(root)
		if rerr != nil {
			// An allowlist entry we cannot resolve allows nothing, and the
			// operator meant it to allow something — say so rather than skip.
			return Decision{Allowed: false,
				Reason: fmt.Sprintf("third-party lane %s denied: glm_allow_repos entry %q cannot be resolved to a real path (fail closed): %v", lane, root, rerr)}
		}
		if within(abs, rabs) {
			return Decision{Allowed: true,
				Reason: "repository context allowlisted: " + rabs}
		}
	}
	return Decision{Allowed: false,
		Reason: fmt.Sprintf("third-party lane %s denied: %s is not inside any allowlisted repo (fail closed — multi-brand isolation)", lane, abs)}
}

// canonical resolves p to the path the filesystem will actually use, so the
// containment test compares real locations rather than spellings.
//
// filepath.Abs alone is LEXICAL. filepath.EvalSymlinks is not enough either —
// see realpath_windows.go for why a junction sails through it. When the leaf
// does not exist yet it cannot itself be a reparse point, but an ANCESTOR can
// be, so we resolve the deepest existing ancestor and re-attach the rest.
// Anything that exists but cannot be resolved is a hard failure: this function's
// callers all fail closed on an error, which is the only safe default for a
// data boundary.
func canonical(p string) (string, error) {
	abs := p
	if isUNC(p) {
		// filepath.Abs MANGLES a UNC path on Windows: \\server\share\x becomes
		// <current-drive>\server\share\x, silently pointing the containment test
		// at a local directory that has nothing to do with the share.
		abs = filepath.Clean(p)
	} else {
		a, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		abs = a
	}
	rest := []string{}
	dir := abs
	for {
		real, rerr := realPath(dir)
		if rerr == nil {
			return filepath.Clean(filepath.Join(append([]string{real}, rest...)...)), nil
		}
		if !os.IsNotExist(rerr) {
			return "", rerr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Nothing along the path exists (a fabricated path in a test, or a
			// drive that is not mounted). Lexical is all there is, and since
			// nothing exists there is no reparse point to hide behind.
			return filepath.Clean(abs), nil
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
		dir = parent
	}
}

// isUNC reports whether p is a UNC path (\\server\share\...).
func isUNC(p string) bool {
	return strings.HasPrefix(filepath.ToSlash(p), "//")
}

// fullyQualified reports whether a path names a location independent of BOTH the
// process's current directory and its current drive.
//
// filepath.IsAbs is not sufficient on Windows in either direction:
//   - "/tmp/x" and "\tmp\x" are NOT absolute (verified), and filepath.Abs
//     resolves them against whatever drive the process is on — so an allowlist
//     entry spelled that way means a different directory depending on where the
//     orchestrator was launched.
//   - "C:x" is drive-RELATIVE and must be refused for the same reason, even
//     though it carries a volume name.
//   - a UNC path is fully qualified but IsAbs reports false for it, so checking
//     IsAbs alone would refuse a legitimate network-share entry.
func fullyQualified(p string) bool {
	return filepath.IsAbs(p) || isUNC(p)
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

// extraFree are the ONLY --extra tokens a third-party dispatch may carry: flags
// that cannot name a filesystem path. The int is how many following tokens the
// flag consumes as its value.
//
// Why an allow-list of harmless flags rather than a gate on path-bearing ones —
// this is the third design of this function, and the two that tried to gate paths
// were both defeated:
//
//	round 2: --add-dir was modelled as arity-1; the shipped binary declares it
//	         `<directories...>`, so every directory after the first was forwarded
//	         ungated while the receipt certified "allowlisted".
//	round 3: even with variadic arity modelled, `--add-dir --output-format <dir>`
//	         slipped through BOTH lists — variadic collection stopped at the
//	         "-" prefix, then --output-format's skip counter ate the directory, so
//	         it was neither gated nor reported. And a RELATIVE --extra value was
//	         resolved against the ORCHESTRATOR's cwd while the child resolves it
//	         against its own, so the gate and the child could disagree about which
//	         directory the token names.
//
// The lesson is not "model the parser more carefully". It is that this gate cannot
// be a second, independent implementation of another program's argument parser:
// commander's real behaviour around variadic options, optional values and
// end-of-flags is subtle, undocumented in places, and free to change in any
// upstream release. So a third-party dispatch may not pass a path-bearing flag AT
// ALL. Repository context reaches a third-party lane through -cwd and the
// allowlist, which is gated at one place with one rule.
//
// Nothing in this repository passes --extra to a third-party lane (mr-goldreplay
// gates its -claude-extra on lane == "claude"), so this closes a hole rather than
// removing a capability. If a path-bearing flag is ever genuinely needed here, the
// honest way to add it is to have the ADAPTER construct the argument from an
// already-gated absolute path, not to re-parse operator text.
var extraFree = map[string]int{
	"--model": 1, "--output-format": 1, "--input-format": 1, "--effort": 1,
	"--permission-mode": 1, "--session-id": 1, "--agents": 1,
	"-p": 0, "--print": 0, "--verbose": 0,
	"--dangerously-skip-permissions": 0, "--strict-mcp-config": 0,
	"--include-partial-messages": 0, "--replay-user-messages": 0, "--safe-mode": 0,
}

// extraOptionalValue are flags whose value is OPTIONAL (`[filter]`), so the token
// after them may or may not belong to them. They are listed separately because a
// fixed skip count is wrong for them in both directions.
var extraOptionalValue = map[string]bool{"--debug": true, "-d": true}

// RefuseExtras returns the --extra tokens that a THIRD-PARTY dispatch must be
// refused for: anything not provably incapable of naming a filesystem path.
// An empty result means every token is on the harmless list.
//
// Deliberately conservative in one direction only: a token this function does not
// recognize is refused, never assumed safe. A false refusal costs the operator an
// error message that names the token; a false accept ships a client's code.
func RefuseExtras(extra []string) []string {
	var bad []string
	for i := 0; i < len(extra); i++ {
		tok := extra[i]
		if tok == "--" {
			// End of flags: everything after it is a positional the child may
			// interpret however it likes. Refuse the lot rather than guess.
			bad = append(bad, extra[i:]...)
			return bad
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			bad = append(bad, tok) // a bare positional; the prompt is passed separately
			continue
		}
		name, _, hasVal := strings.Cut(tok, "=")
		if extraOptionalValue[name] {
			// Consume a following value only when it cannot be a flag itself.
			if !hasVal && i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-") {
				i++
			}
			continue
		}
		skip, ok := extraFree[name]
		if !ok {
			bad = append(bad, tok)
			continue
		}
		if !hasVal {
			// Do not skip past a token that looks like a flag: if the operator
			// wrote `--model --verbose`, the second token is its own flag and
			// must still be judged.
			for n := 0; n < skip && i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-"); n++ {
				i++
			}
		}
	}
	return bad
}
