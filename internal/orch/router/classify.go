package router

import "strings"

// Classify is the FALLBACK-ONLY heuristic: the §6c brain normally KNOWS the
// class and passes --class; this keeps `route` usable on a bare --desc.
// Deterministic keyword/attribute rules, NO LLM (hot-path law). Returns the
// class and a ruleTag naming the rule that fired (receipt/debug substrate).
//
// Precedence (first match wins): leading mechanical verb → latency-sensitive →
// long-context by tokens → terminal keywords → formal-math → verify-gate →
// hard-repo → default. The default is QUALITY-FIRST (S2R-11): HardRepo (rank-1
// Opus), NOT the plan's original workhorse-GLM — an unclassifiable task must
// not silently fall to the cheap lane; the caller's Reason nudges --class.
func Classify(desc string, ctxTokens int64, latencySensitive bool) (Class, string) {
	d := strings.ToLower(desc)

	// Leading mechanical verb (nudge.go verbWindow pattern): the first
	// meaningful word signals a grunt task. summarize + small ctx is the
	// doc-summarize special case (≤32K); everything else in the set is
	// mechanical-text.
	if v, ok := leadingVerb(d); ok {
		if v == "summarize" && ctxTokens <= 32_000 {
			return DocSummarize, "verb-summarize-small-ctx"
		}
		return MechanicalText, "verb-mechanical"
	}

	if latencySensitive {
		return LatencyIter, "latency-sensitive"
	}
	if ctxTokens > 300_000 {
		return LongContext, "ctx-over-300k"
	}
	if containsAny(d, "terminal", "shell", "cli", "script", "one-shot command") {
		return TerminalBounded, "kw-terminal"
	}
	if containsAny(d, "prove", "proof", "theorem") {
		return FormalMath, "kw-formal-math"
	}
	if containsAny(d, "verify", "yes/no", "gate", "check whether") {
		return VerifyGate, "kw-verify-gate"
	}
	if containsAny(d, "refactor", "multi-file", "architecture", "migration") {
		return HardRepo, "kw-hard-repo"
	}
	// S2R-11: quality-first default. The route Reason (built by the caller)
	// notes the brain should pass --class for precision.
	return HardRepo, "heuristic-default-quality-first"
}

// mechanicalVerbs is the grammar-constrained grunt-task verb set (the local /
// GLM cheap tiers own these classes). Order-independent membership.
var mechanicalVerbs = []string{"summarize", "classify", "extract", "triage", "ocr", "transcribe", "label", "tag"}

// leadingVerb returns the first mechanical verb if the description LEADS with
// one (verbWindow: the task's opening word is the strongest signal). A verb
// buried mid-sentence is not a reliable grunt signal, so only the first token
// counts.
func leadingVerb(d string) (string, bool) {
	fields := strings.Fields(d)
	if len(fields) == 0 {
		return "", false
	}
	first := strings.Trim(fields[0], ".,:;!?")
	for _, v := range mechanicalVerbs {
		if first == v {
			return v, true
		}
	}
	return "", false
}

func containsAny(d string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(d, s) {
			return true
		}
	}
	return false
}
