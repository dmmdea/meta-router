package claudelane

import (
	"fmt"
	"slices"
)

type RunReq struct {
	Prompt, Model, Effort, CWD string
	TimeoutSec                 int
	Extra                      []string // operator passthrough (R11); validated against the forbidden list
	// Env entries are APPENDED to os.Environ() for the child (glm lane: base
	// URL + auth token + model pins). Empty = claude-lane behavior, unchanged.
	// Never placed in argv, never logged (R10).
	Env []string
}

// forbidden flags: --bare never reads OAuth (R10 violation → silent API-key
// dependency); --bg is hard-rejected with -p since 2.1.198.
var forbidden = []string{"--bare", "--bg"}

func BuildArgs(r RunReq) ([]string, error) {
	if r.Model == "" {
		return nil, fmt.Errorf("model is required: unpinned claude -p defaults to Sonnet 5 (fact refresh §3)")
	}
	for _, x := range r.Extra {
		if slices.Contains(forbidden, x) {
			return nil, fmt.Errorf("forbidden flag %s (see intent R10 / fact refresh hard rules)", x)
		}
	}
	args := []string{"-p", r.Prompt, "--model", r.Model, "--output-format", "json"}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}
	return append(args, r.Extra...), nil
}
