package policyzoo

import (
	"testing"

	"github.com/dmmdea/meta-router/internal/policyeval"
)

func mkTask(id, base, prompt string) Task { return Task{ID: id, Class: "c", Prompt: prompt, BaseLane: base} }

func TestFloorBumpsOnlyUpward(t *testing.T) {
	// A structurally complex prompt on a cheap base gets floored; a claude
	// base is never demoted; a simple prompt is never touched.
	complex := "1. run x\n2. deploy y\n```\ncode\n```\nfix main.go"
	for _, c := range ComplexityFloorGrid() {
		up := c.Route(mkTask("a", "local", complex))
		if laneTier[up] < laneTier["local"] {
			t.Fatalf("%s demoted below base", c.Desc)
		}
		if got := c.Route(mkTask("b", "claude", complex)); got != "claude" {
			t.Fatalf("%s changed a claude base to %s (floors never demote)", c.Desc, got)
		}
		if got := c.Route(mkTask("d", "glm", "short question")); got != "glm" {
			t.Fatalf("%s bumped a simple prompt (score 0) to %s", c.Desc, got)
		}
	}
}

func TestCtxFloorTriggersOnLength(t *testing.T) {
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'x'
	}
	found := false
	for _, c := range CtxFloorGrid() {
		if c.Route(mkTask("a", "glm", string(long))) == "codex" {
			found = true
		}
		if got := c.Route(mkTask("b", "glm", "short")); got != "glm" {
			t.Fatalf("%s floored a short prompt", c.Desc)
		}
	}
	if !found {
		t.Fatal("no ctx-floor config floors a 4000-char prompt to codex")
	}
}

func TestSelectBestPicksByTuningPassRateThenCheaper(t *testing.T) {
	tb := policyeval.NewTable()
	tb.Add("t1", "glm", true)
	tb.Add("t1", "codex", true)
	tasks := []Task{mkTask("t1", "glm", "short")}
	cands := []Candidate{
		{Family: "f", Desc: "stay", Route: func(t Task) string { return t.BaseLane }},
		{Family: "f", Desc: "bump", Route: func(t Task) string { return "codex" }},
	}
	best, ev := SelectBest(cands, tb, tasks)
	if best.Desc != "stay" {
		t.Fatalf("tie must break to lower claude-fraction then lexical Desc: got %s", best.Desc)
	}
	if ev.PassRate != 1.0 {
		t.Fatalf("PassRate=%v want 1.0", ev.PassRate)
	}
}

func TestSelectBestPrefersHigherPassRate(t *testing.T) {
	tb := policyeval.NewTable()
	tb.Add("t1", "glm", false)
	tb.Add("t1", "codex", true)
	tasks := []Task{mkTask("t1", "glm", "short")}
	cands := []Candidate{
		{Family: "f", Desc: "stay", Route: func(t Task) string { return t.BaseLane }},
		{Family: "f", Desc: "bump", Route: func(t Task) string { return "codex" }},
	}
	best, _ := SelectBest(cands, tb, tasks)
	if best.Desc != "bump" {
		t.Fatalf("higher tuning pass-rate must win: got %s", best.Desc)
	}
}

func TestPolicyOfAbstainsOnUnknownTask(t *testing.T) {
	p := PolicyOf(Candidate{Family: "f", Desc: "x", Route: func(t Task) string { return "glm" }}, map[string]Task{})
	if got := p("never-seen"); got != "" {
		t.Fatalf("unknown task must abstain (\"\"), got %q", got)
	}
}
