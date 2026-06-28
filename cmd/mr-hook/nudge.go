package main

import "strings"

// offloadVerbs are imperative verbs that signal mechanical text processing the
// free local offload_* tools handle well.
var offloadVerbs = map[string]bool{
	"summarize": true, "summarise": true, "classify": true, "categorize": true,
	"categorise": true, "extract": true, "triage": true, "transcribe": true,
	"ocr": true, "label": true, "tag": true,
}

// offloadNudgeText is the one-line additionalContext nudge.
const offloadNudgeText = "meta-router — this looks like mechanical text work (summarize/classify/extract/triage over pasted text). Consider the free local offload_* MCP tools (offload_summarize / offload_classify / offload_extract / offload_triage) to keep it off the cloud context."

// verbWindow is how many leading tokens are scanned for an offload verb. An
// offload request is an imperative ("Summarize the following log: …") so the verb
// leads; a verb buried mid-sentence is conversation that merely mentions it
// ("…whether we should summarize our strategy…"), not a task to offload.
const verbWindow = 6

// offloadNudge reports whether the prompt is a high-precision offload candidate:
// it requires an offload imperative verb in LEADING position (first verbWindow
// tokens) AND a substantial pasted block, so short asks ("summarize our chat")
// and verb-less or verb-buried prompts don't fire. Pure function of the prompt —
// no embedding, no I/O.
func offloadNudge(prompt string) bool {
	hasVerb := false
	for i, tok := range strings.Fields(strings.ToLower(prompt)) {
		if i >= verbWindow {
			break
		}
		tok = strings.Trim(tok, ".,:;!?\"'`)(")
		if offloadVerbs[tok] {
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		return false
	}
	return len(prompt) >= 400 || strings.Count(prompt, "\n") >= 4
}

// appendNudge appends the nudge to ctx (returns the bare nudge when ctx is empty).
func appendNudge(ctx string) string {
	if ctx == "" {
		return offloadNudgeText
	}
	return ctx + "\n\n" + offloadNudgeText
}
