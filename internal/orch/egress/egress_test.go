package egress

import (
	"os"
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

// CRITICAL (review 2026-07-25): an empty CWD does NOT mean "no repo context".
// os/exec runs the child in the PARENT's cwd when Cmd.Dir is empty, so running
// `mr-orchestrate run --lane glm` from inside a client checkout exported that
// checkout while the receipt recorded "prompt-only". Plan must resolve the
// INHERITED cwd and, when it is not allowlisted, make prompt-only TRUE by
// substituting a neutral directory rather than trusting the assumption.
func TestPlanNeutralisesAnUnallowlistedInheritedCwd(t *testing.T) {
	dir, cleanup, d := Plan("glm", "", Options{}) // no allowlist at all
	defer cleanup()
	if !d.Allowed {
		t.Fatalf("prompt-only must remain usable: %+v", d)
	}
	if dir == "" {
		t.Fatal("Plan must return an EXPLICIT directory; an empty one silently inherits the caller's cwd")
	}
	wd, _ := os.Getwd()
	if dir == wd {
		t.Fatalf("the inherited cwd must not be used when it is not allowlisted (got %s)", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the neutral directory must exist and be EMPTY (err=%v, %d entries)", err, len(entries))
	}
	if !strings.Contains(d.Reason, "ENFORCED") {
		t.Fatalf("the receipt reason must say the guarantee was enforced, not assumed: %s", d.Reason)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the neutral directory")
	}
}

// When the inherited cwd IS allowlisted, Plan uses it (no pointless temp dir).
func TestPlanUsesAnAllowlistedInheritedCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, cleanup, d := Plan("glm", "", Options{AllowRepos: []string{wd}})
	defer cleanup()
	if !d.Allowed || dir != wd {
		t.Fatalf("an allowlisted inherited cwd must be used as-is: dir=%s %+v", dir, d)
	}
}

// An EXPLICIT non-allowlisted cwd is a deliberate request for that repo's
// context and must be refused outright — never silently neutralised.
func TestPlanRefusesExplicitNonAllowlistedCwd(t *testing.T) {
	_, cleanup, d := Plan("glm", filepath.FromSlash("D:/dev/pepsdubai/client"), Options{
		AllowRepos: []string{filepath.FromSlash("D:/Dev/dmmdea/meta-router")},
	})
	defer cleanup()
	if d.Allowed {
		t.Fatalf("an explicitly requested non-allowlisted repo must be refused: %+v", d)
	}
}

// --extra --add-dir is a second export channel the cwd gate never saw.
func TestAddDirsExtractsBothForms(t *testing.T) {
	got := AddDirs([]string{"--add-dir", "D:/a", "--verbose", "--add-dir=D:/b"})
	if len(got) != 2 || got[0] != "D:/a" || got[1] != "D:/b" {
		t.Fatalf("both --add-dir forms must be extracted, got %v", got)
	}
	if len(AddDirs([]string{"--add-dir"})) != 0 {
		t.Fatal("a dangling --add-dir must not panic or invent a path")
	}
}
