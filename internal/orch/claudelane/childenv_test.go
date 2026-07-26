package claudelane

import (
	"strings"
	"testing"
)

// Exercises the SAME helper Run composes its environment with (childEnv), rather
// than a local reimplementation of the logic — so the scrub-then-pin ORDER is
// pinned where it is defined.
//
// What this does NOT prove, stated so the claim is not read as wider than it is:
// it calls childEnv directly, so it cannot detect Run ceasing to call childEnv,
// or a later cmd.Env assignment overwriting the result. Those are structural
// facts about the call site, and the B13 canary is what checks them.
func TestChildEnvScrubsAmbientButKeepsLanePins(t *testing.T) {
	ambient := []string{
		"PATH=C:/Windows",
		"ANTHROPIC_API_KEY=sk-ant-leaked", // would redirect a subscription dispatch to metered spend
		"CLAUDE_CODE_SIMPLE=1",            // equivalent to the banned --bare flag
		"ANTHROPIC_BASE_URL=https://leak",
	}
	// The GLM lane's deliberate pins, appended AFTER the scrub.
	pins := []string{"ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic", "ANTHROPIC_AUTH_TOKEN=glm-token"}
	got := strings.Join(childEnv(ambient, pins), "\n")

	for _, banned := range []string{"sk-ant-leaked", "CLAUDE_CODE_SIMPLE", "https://leak"} {
		if strings.Contains(got, banned) {
			t.Fatalf("ambient %q must never reach the child: %s", banned, got)
		}
	}
	if !strings.Contains(got, "api.z.ai") || !strings.Contains(got, "glm-token") {
		t.Fatalf("the lane's deliberate pins must survive the scrub: %s", got)
	}
	if !strings.Contains(got, "PATH=") {
		t.Fatalf("load-bearing vars must survive: %s", got)
	}
}

// The plain claude path passes NO pins — it previously left cmd.Env nil and so
// inherited everything, which is the actual live exposure.
func TestChildEnvScrubsEvenWithNoPins(t *testing.T) {
	got := childEnv([]string{"ANTHROPIC_API_KEY=sk", "PATH=x"}, nil)
	if strings.Contains(strings.Join(got, "\n"), "ANTHROPIC_API_KEY") {
		t.Fatalf("the no-pins path must still be scrubbed, got %v", got)
	}
	if len(got) != 1 || got[0] != "PATH=x" {
		t.Fatalf("everything else must survive, got %v", got)
	}
}
