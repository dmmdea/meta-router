package egress

import (
	"os"
	"path/filepath"
	"runtime"
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

// --extra is a second export channel with the same reach as cwd.
//
// The FIRST version of this extractor modelled --add-dir as arity-1. The shipped
// claude binary declares `--add-dir <directories...>` — variadic — so
// `--add-dir <allowed> <client>` passed the gate with only <allowed> inspected,
// handed <client> to a PRC-hosted provider, and the receipt certified the
// dispatch as "repository context allowlisted". A plain typo
// (`--add-dir ../lib ../shared`) was enough. These pin the real arity.
func TestExtrasConsumesTheWholeVariadicRun(t *testing.T) {
	paths, un := Extras([]string{"--add-dir", "D:/a", "D:/b", "D:/c", "--verbose"})
	if len(paths) != 3 || paths[0] != "D:/a" || paths[1] != "D:/b" || paths[2] != "D:/c" {
		t.Fatalf("a variadic flag consumes every following non-flag token, got %v", paths)
	}
	if len(un) != 0 {
		t.Fatalf("--verbose cannot carry a path and must not be unaccounted: %v", un)
	}
}

func TestExtrasBothAttachedForms(t *testing.T) {
	paths, un := Extras([]string{"--add-dir=D:/a", "--plugin-dir", "D:/b"})
	if len(paths) != 2 || paths[0] != "D:/a" || paths[1] != "D:/b" {
		t.Fatalf("both = and space forms must be extracted, got %v", paths)
	}
	if len(un) != 0 {
		t.Fatalf("unexpected unaccounted: %v", un)
	}
	if p, _ := Extras([]string{"--add-dir"}); len(p) != 0 {
		t.Fatalf("a dangling --add-dir must not panic or invent a path, got %v", p)
	}
}

// A non-variadic path flag must NOT swallow the token after its value.
func TestExtrasNonVariadicTakesExactlyOne(t *testing.T) {
	paths, un := Extras([]string{"--plugin-dir", "D:/a", "stray"})
	if len(paths) != 1 || paths[0] != "D:/a" {
		t.Fatalf("--plugin-dir takes one value, got %v", paths)
	}
	if len(un) != 1 || un[0] != "stray" {
		t.Fatalf("the stray positional must be reported as unaccounted, got %v", un)
	}
}

// The other variadic path-bearing flags the shipped binary declares. Each was
// invisible to the old extractor, which knew only --add-dir.
func TestExtrasCoversTheOtherVariadicPathFlags(t *testing.T) {
	for _, flag := range []string{"--mcp-config", "--file", "--sparse"} {
		paths, un := Extras([]string{flag, "D:/one", "D:/two"})
		if len(paths) != 2 {
			t.Errorf("%s is variadic and path-bearing; got paths=%v un=%v", flag, paths, un)
		}
	}
}

// Deny-by-default: a flag this gate does not model must be REPORTED, not
// assumed harmless. The CLI ships 82 options and grows; a gate that knows only
// the dangerous ones fails silently the first time a new one appears.
func TestExtrasReportsUnmodelledFlags(t *testing.T) {
	_, un := Extras([]string{"--some-future-flag", "value"})
	if len(un) == 0 {
		t.Fatal("an unmodelled flag must be unaccounted so the caller can refuse it")
	}
	if _, un := Extras([]string{"--dangerously-skip-permissions"}); len(un) != 0 {
		t.Fatalf("a known path-free flag must not be flagged: %v", un)
	}
}

// A RELATIVE allowlist entry would resolve against whatever directory the
// orchestrator is standing in — "." self-allowlists every checkout in turn.
func TestRelativeAllowlistEntryIsRefused(t *testing.T) {
	d := Check("glm", filepath.FromSlash("D:/dev/anything"), Options{AllowRepos: []string{"."}})
	if d.Allowed {
		t.Fatal("a relative allowlist entry must never allow anything")
	}
	if !strings.Contains(d.Reason, "not fully qualified") {
		t.Fatalf("the denial must name the reason: %s", d.Reason)
	}
}

// Junctions: EvalSymlinks reports a Windows junction as unchanged with a nil
// error, so the old link guard was a self-assignment and a junction inside an
// allowlisted repo re-exported its target. mklink /J needs no elevation, unlike
// mklink /D, so the only case the old guard caught was the one requiring admin.
func TestJunctionInsideAllowlistedRepoDoesNotEscape(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	secret := filepath.Join(base, "client-secret")
	for _, d := range []string{allowed, secret} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(allowed, "link")
	if !makeDirLink(t, link, secret) {
		t.Skip("no unprivileged directory-link mechanism on this platform")
	}
	d := Check("glm", link, Options{AllowRepos: []string{allowed}})
	if d.Allowed {
		t.Fatalf("a link inside an allowlisted repo must be judged by its TARGET, not its path: %+v", d)
	}
}

// Same shape, but the link points INSIDE the allowlisted repo: resolution must
// not turn into a blanket denial of every link.
func TestLinkToAnAllowlistedTargetStillPasses(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	inner := filepath.Join(allowed, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "shortcut")
	if !makeDirLink(t, link, inner) {
		t.Skip("no unprivileged directory-link mechanism on this platform")
	}
	if d := Check("glm", link, Options{AllowRepos: []string{allowed}}); !d.Allowed {
		t.Fatalf("a link whose target IS allowlisted must be allowed: %+v", d)
	}
}

// Path shapes Windows treats specially. filepath.IsAbs alone is wrong in BOTH
// directions here, so each is pinned: driveless-rooted and drive-relative entries
// depend on where the orchestrator was launched and must be refused; a UNC path
// is fully qualified and must not be mistaken for relative.
func TestFullyQualifiedPathShapes(t *testing.T) {
	for _, c := range []struct {
		p    string
		want bool
	}{
		{`C:\Dev\repo`, true},
		{`\\server\share\repo`, true}, // UNC: IsAbs reports false for this
		{`//server/share/repo`, true},
		{"/tmp/repo", runtime.GOOS != "windows"}, // absolute on POSIX, drive-dependent on Windows
		{".", false},
		{"..", false},
		{"relative/path", false},
		{`C:repo`, false}, // drive-RELATIVE, despite carrying a volume name
	} {
		if got := fullyQualified(c.p); got != c.want {
			t.Errorf("fullyQualified(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}

// An UNREACHABLE UNC path must fail CLOSED, and — the part that matters — must
// never be silently rewritten onto a LOCAL drive. filepath.Abs turns
// \\host\share\x into <current-drive>\host\share\x, which is a real directory
// name on the local disk: if that rewrite reached the containment test, an
// allowlist entry naming a network share could be satisfied by a local folder
// that merely happens to be spelled the same.
//
// Note this test talks to the network stack and takes a couple of seconds to
// time out; that delay is the OS refusing to resolve the share, which is exactly
// the condition being asserted.
func TestUnreachableUNCFailsClosedAndIsNotLocalised(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("UNC paths are a Windows shape")
	}
	const p = `\\nonexistent-host\share\repo`
	got, err := canonical(p)
	if err == nil && !isUNC(got) {
		t.Fatalf("canonical rewrote a UNC path onto a local drive: %q", got)
	}
	// Whatever canonical decided, the gate's answer must be a denial: an
	// unresolvable location cannot be proven to be inside the allowlist.
	if d := Check("glm", p, Options{AllowRepos: []string{`\\nonexistent-host\share`}}); d.Allowed {
		t.Fatalf("an unresolvable UNC path must fail closed: %+v", d)
	}
}
