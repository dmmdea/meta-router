package main

import (
	"strings"
	"unicode"
)

// taskNotificationTag opens every background-task notification Claude Code delivers
// through UserPromptSubmit: a finished background command, a finished subagent, a Monitor
// event. A notification that arrives while the model is busy is queued and then delivered
// with the same text, so one prefix covers both paths; measured on real transcripts, all
// 1,710 queued notifications start with it, and the usage log's prompt hashes match the
// transcript text byte for byte.
const taskNotificationTag = "<task-notification>"

// isMachineTurn reports whether the prompt is a background-task notification rather than
// something the operator typed. The skill hint answers the operator's request, so on these
// turns it stays silent: one long session had 25 hints on notifications against 7 on real
// prompts, and each one stays in context for the rest of the session.
func isMachineTurn(prompt string) bool {
	return strings.HasPrefix(strings.TrimLeftFunc(prompt, unicode.IsSpace), taskNotificationTag)
}
