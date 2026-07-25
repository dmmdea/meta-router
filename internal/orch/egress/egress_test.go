package egress

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSubscriptionLanesAreNotGated(t *testing.T) {
	for _, lane := range []string{"claude", "codex", "local"} {
		if d := Check(lane, "C:/Dev/anything", Options{}); !d.Allowed {
			t.Fatalf("%s is not a third-party lane and must never be gated: %+v", lane, d)
		}
	}
}

// The live defect: GLM is rank 1 for coding classes and runs with cmd.Dir set
// inside a real repo. With no allowlist configured that export must be REFUSED,
// not silently performed.
func TestRepoContextDeniedByDefault(t *testing.T) {
	d := Check("glm", "D:/dev/pepsdubai/readypep-store", Options{})
	if d.Allowed {
		t.Fatal("repository context to a third-party lane must deny by default")
	}
	if !strings.Contains(d.Reason, "glm_allow_repos") {
		t.Fatalf("the denial must tell the operator how to fix it: %s", d.Reason)
	}
}

func TestAllowlistedRepoIsAllowed(t *testing.T) {
	root := filepath.FromSlash("D:/Dev/dmmdea/meta-router")
	sub := filepath.Join(root, "internal", "orch")
	d := Check("glm", sub, Options{AllowRepos: []string{root}})
	if !d.Allowed {
		t.Fatalf("a subdirectory of an allowlisted repo must be allowed: %+v", d)
	}
	if !strings.Contains(d.Reason, "allowlisted") {
		t.Fatalf("an allow must name its basis: %s", d.Reason)
	}
}

// A sibling whose path merely shares a PREFIX with an allowlisted repo must not
// slip through: "…/client-secret" is not inside "…/client".
func TestPrefixSiblingIsNotInside(t *testing.T) {
	d := Check("glm", filepath.FromSlash("D:/Dev/client-secret"), Options{
		AllowRepos: []string{filepath.FromSlash("D:/Dev/client")},
	})
	if d.Allowed {
		t.Fatalf("a prefix-sharing sibling must not be treated as inside: %+v", d)
	}
}

// The brand-isolation case, stated concretely: an allowlisted personal repo
// must not license a client checkout on another path.
func TestOtherRepoStillDenied(t *testing.T) {
	d := Check("glm", filepath.FromSlash("D:/dev/pepsdubai/peptidoteca"), Options{
		AllowRepos: []string{filepath.FromSlash("D:/Dev/dmmdea/meta-router")},
	})
	if d.Allowed {
		t.Fatalf("a non-allowlisted repo must stay denied: %+v", d)
	}
	if !strings.Contains(d.Reason, "multi-brand isolation") {
		t.Fatalf("the denial should name the rule it enforces: %s", d.Reason)
	}
}

func TestPromptOnlyAllowedUnlessClosed(t *testing.T) {
	if d := Check("glm", "", Options{}); !d.Allowed {
		t.Fatalf("prompt-only carries no repo context and is allowed by default: %+v", d)
	}
	if d := Check("glm", "", Options{PromptOnlyDenied: true}); d.Allowed {
		t.Fatal("PromptOnlyDenied must close the prompt-only path too")
	}
}

func TestUnresolvablePathFailsClosed(t *testing.T) {
	// A path with a NUL byte cannot be resolved on any platform.
	if d := Check("glm", "bad\x00path", Options{AllowRepos: []string{"D:/Dev"}}); d.Allowed {
		t.Fatal("an unresolvable working directory must fail closed")
	}
}

func TestFutureFreeLanesInheritTheGate(t *testing.T) {
	// The approved free-lane members must be gated by the SAME rule rather than
	// each re-deciding (Groq/Cloudflare seated 2026-07-23; Gemini gated on it).
	for _, lane := range []string{"groq", "cloudflare", "gemini", "nim"} {
		if !ThirdParty(lane) {
			t.Fatalf("%s must be treated as third-party", lane)
		}
		if d := Check(lane, "D:/dev/anything", Options{}); d.Allowed {
			t.Fatalf("%s must deny repo context by default: %+v", lane, d)
		}
	}
}
