package router

import "testing"

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name             string
		desc             string
		ctx              int64
		latencySensitive bool
		want             Class
		wantTag          string
	}{
		{"mechanical verb summarize small ctx", "summarize this build log", 8000, false, DocSummarize, "verb-summarize-small-ctx"},
		{"mechanical verb classify", "classify these support tickets", 2000, false, MechanicalText, "verb-mechanical"},
		{"mechanical verb extract", "extract the fields from this receipt", 1000, false, MechanicalText, "verb-mechanical"},
		{"mechanical verb triage", "triage these logs", 500, false, MechanicalText, "verb-mechanical"},
		{"summarize but large ctx is doc-summarize? no — >32K falls out of doc-summarize", "summarize this huge doc", 40000, false, MechanicalText, "verb-mechanical"},
		{"latency sensitive wins", "iterate quickly on this", 1000, true, LatencyIter, "latency-sensitive"},
		{"long context by tokens", "read the whole repo and answer", 400000, false, LongContext, "ctx-over-300k"},
		{"terminal keyword", "fix the failing PowerShell script one-shot command", 3000, false, TerminalBounded, "kw-terminal"},
		{"terminal shell keyword", "run this shell command", 1000, false, TerminalBounded, "kw-terminal"},
		{"formal math keyword", "prove this theorem", 2000, false, FormalMath, "kw-formal-math"},
		{"verify gate keyword", "verify whether the tests pass yes/no", 1000, false, VerifyGate, "kw-verify-gate"},
		{"hard repo keyword refactor", "refactor admission across 4 packages", 5000, false, HardRepo, "kw-hard-repo"},
		{"hard repo keyword migration", "run the database migration multi-file", 5000, false, HardRepo, "kw-hard-repo"},
		// S2R-11: the unknown/default class is QUALITY-FIRST (HardRepo, rank-1
		// Opus), NOT workhorse-GLM. The plan's `default -> workhorse-coding` is
		// OVERRIDDEN. The ruleTag marks it and the caller's route Reason nudges
		// the brain to pass --class.
		{"unknown default is quality-first", "do something vague and unusual", 5000, false, HardRepo, "heuristic-default-quality-first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, tag := Classify(tc.desc, tc.ctx, tc.latencySensitive)
			if c != tc.want {
				t.Fatalf("class: got %q want %q (tag %q)", c, tc.want, tag)
			}
			if tag != tc.wantTag {
				t.Fatalf("ruleTag: got %q want %q", tag, tc.wantTag)
			}
		})
	}
}
