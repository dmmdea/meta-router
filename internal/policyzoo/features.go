// Package policyzoo holds W3's candidate routing policies: deployable-shaped
// functions from OBSERVABLE task features (prompt text + the shipped router's
// neutral-state pick) to a lane. Candidates never see oracle cells; the table
// is used only by the tuning-split sweep (SelectBest) and heldout scoring —
// the B'2 protocol. B'2's verdict scopes this package: class-level signals
// are exhausted; candidates here chase finer-than-class features.
package policyzoo

import "regexp"

// Features are cheap, deterministic prompt observables. Known consistent-bias
// noise (harmless because thresholds are tuned on this same extractor):
// "x_test.go" counts as one FileRef and its "test" stem as a ToolVerb; URLs
// with file-like endings count as FileRefs.
type Features struct {
	CodeFences    int
	NumberedSteps int
	ToolVerbs     int
	FileRefs      int
}

// Score is the pinned complexity proxy: structural markers, not length.
func (f Features) Score() int { return f.CodeFences + f.NumberedSteps + f.ToolVerbs + f.FileRefs }

var (
	fenceRe = regexp.MustCompile("```")
	stepRe  = regexp.MustCompile(`(?m)^\s*\d+[.)]\s`)
	verbRe  = regexp.MustCompile(`(?i)\b(run|execute|deploy|install|build|test|tests|fetch|browse|scrape|migrate|refactor|debug|benchmark|profile)\b`)
	fileRe  = regexp.MustCompile(`\S+\.(go|py|ts|tsx|js|rs|md|json|yaml|yml|toml|sh|ps1|sql)\b`)
)

// Extract computes Features. Pure, no I/O. (Prompt LENGTH is not a Feature:
// the ctx-floor family reads len(prompt) directly — bytes, by design.)
func Extract(prompt string) Features {
	return Features{
		CodeFences:    len(fenceRe.FindAllString(prompt, -1)) / 2,
		NumberedSteps: len(stepRe.FindAllString(prompt, -1)),
		ToolVerbs:     len(verbRe.FindAllString(prompt, -1)),
		FileRefs:      len(fileRe.FindAllString(prompt, -1)),
	}
}
