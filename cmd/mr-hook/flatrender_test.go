package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// Review 2026-07-30, MAJOR: agent entries surfaced as "agent:context-engineer"
// with the trailer "Invoke one via the Skill tool or its slash command" — an ID
// no tool accepts (it matches the plugin pack:skill grammar, so the model would
// call Skill("agent:...") and fail) and an instruction that is a lie for all 35
// agents. These pin the kind-aware rendering.

func fixtureByID(skills ...catalog.Skill) (map[string]catalog.Skill, []string) {
	m := map[string]catalog.Skill{}
	var ids []string
	for _, s := range skills {
		m[s.ID] = s
		ids = append(ids, s.ID)
	}
	return m, ids
}

// Production today surfaces only skills; that output must be BYTE-identical to
// the pre-flat-roots rendering, or this change is not behavior-inert.
func TestFormatContextSkillsOnlyIsByteIdentical(t *testing.T) {
	m, ids := fixtureByID(
		catalog.Skill{ID: "gstack-qa", Source: "skills", Description: "QA a web app."},
		catalog.Skill{ID: "superpowers:brainstorming", Source: "superpowers", Description: "Explore intent first."},
	)
	got := formatContext(m, ids)
	want := "meta-router — relevant installed skills for this task:\n" +
		"- gstack-qa (skills): QA a web app.\n" +
		"- superpowers:brainstorming (superpowers): Explore intent first.\n" +
		"Invoke one via the Skill tool or its slash command if it fits."
	if got != want {
		t.Fatalf("skills-only output changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatContextAgentRendersBareNameAndAgentTrailer(t *testing.T) {
	m, ids := fixtureByID(
		catalog.Skill{ID: "agent:context-engineer", Source: "agents", Description: "Audit context overhead."},
	)
	got := formatContext(m, ids)
	if strings.Contains(got, "agent:context-engineer") {
		t.Fatalf("the raw agent: ID must never be surfaced (no tool accepts it): %q", got)
	}
	if !strings.Contains(got, "- context-engineer (agent): ") {
		t.Fatalf("agent must render as bare name labeled (agent): %q", got)
	}
	if !strings.Contains(got, "Agent tool (subagent_type = the listed name)") {
		t.Fatalf("agent trailer clause missing: %q", got)
	}
}

func TestFormatContextCommandKeepsSlashForm(t *testing.T) {
	m, ids := fixtureByID(
		catalog.Skill{ID: "/insights", Source: "commands", Description: "Learning analytics."},
	)
	got := formatContext(m, ids)
	if !strings.Contains(got, "- /insights (commands): ") {
		t.Fatalf("command must surface its typeable slash form: %q", got)
	}
	if strings.Contains(got, "(agent)") || strings.Contains(got, "Agent tool") {
		t.Fatalf("no agent clause without agents: %q", got)
	}
}
