package childenv

import (
	"strings"
	"testing"
)

// Ambient GitHub tokens must never reach a lane child: the copilot CLI's env
// precedence (COPILOT_GITHUB_TOKEN > GH_TOKEN > GITHUB_TOKEN) plus gh's own
// GH_TOKEN-over-keyring rule would let a stray shell export bill an arbitrary
// account from inside ANY lane. The lane's deliberate token is appended AFTER
// Scrub by its runner, so this scrub cannot break the copilot lane itself.
func TestScrubStripsGitHubTokensAndCopilotHome(t *testing.T) {
	in := []string{
		"PATH=/x",
		"COPILOT_GITHUB_TOKEN=leak1",
		"GH_TOKEN=leak2",
		"GITHUB_TOKEN=leak3",
		"COPILOT_HOME=/somewhere/else",
	}
	out := Scrub(in)
	joined := strings.Join(out, "\n")
	for _, bad := range []string{"leak1", "leak2", "leak3", "COPILOT_HOME"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("Scrub leaked %q: %v", bad, out)
		}
	}
	if !strings.Contains(joined, "PATH=/x") {
		t.Fatalf("Scrub dropped an innocent var: %v", out)
	}
}
