package egress

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// absTest builds a path that is FULLY QUALIFIED on the host platform.
//
// Hardcoded "D:/Dev/..." literals are absolute on Windows and RELATIVE on Linux —
// where the allowlist guard correctly refuses them for depending on the process's
// current directory. That made three of these tests pass locally and fail on the
// POSIX CI runner (2026-07-26). The gate's behaviour was right; the fixtures were
// Windows-shaped. Same environment-assumption class this repo has now been bitten
// by three times, so the shape is built rather than written.
func absTest(parts ...string) string {
	base := "/"
	if runtime.GOOS == "windows" {
		base = `D:\`
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

func TestSubscriptionLanesAreNotGated(t *testing.T) {
	for _, lane := range []string{"claude", "codex", "local"} {
		if d := Check(lane, absTest("Dev", "anything"), Options{}); !d.Allowed {
			t.Fatalf("%s is not a third-party lane and must never be gated: %+v", lane, d)
		}
	}
}

// The live defect: GLM is rank 1 for coding classes and runs with cmd.Dir set
// inside a real repo. With no allowlist configured that export must be REFUSED,
// not silently performed.
func TestRepoContextDeniedByDefault(t *testing.T) {
	d := Check("glm", absTest("dev", "pepsdubai", "readypep-store"), Options{})
	if d.Allowed {
		t.Fatal("repository context to a third-party lane must deny by default")
	}
	if !strings.Contains(d.Reason, "glm_allow_repos") {
		t.Fatalf("the denial must tell the operator how to fix it: %s", d.Reason)
	}
}

func TestAllowlistedRepoIsAllowed(t *testing.T) {
	root := absTest("Dev", "dmmdea", "meta-router")
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
	d := Check("glm", absTest("Dev", "client-secret"), Options{
		AllowRepos: []string{absTest("Dev", "client")},
	})
	if d.Allowed {
		t.Fatalf("a prefix-sharing sibling must not be treated as inside: %+v", d)
	}
}

// The brand-isolation case, stated concretely: an allowlisted personal repo
// must not license a client checkout on another path.
func TestOtherRepoStillDenied(t *testing.T) {
	d := Check("glm", absTest("dev", "pepsdubai", "peptidoteca"), Options{
		AllowRepos: []string{absTest("Dev", "dmmdea", "meta-router")},
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
	if d := Check("glm", "bad\x00path", Options{AllowRepos: []string{absTest("Dev")}}); d.Allowed {
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
		if d := Check(lane, absTest("dev", "anything"), Options{}); d.Allowed {
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
	_, cleanup, d := Plan("glm", absTest("dev", "pepsdubai", "client"), Options{
		AllowRepos: []string{absTest("Dev", "dmmdea", "meta-router")},
	})
	defer cleanup()
	if d.Allowed {
		t.Fatalf("an explicitly requested non-allowlisted repo must be refused: %+v", d)
	}
}

// --extra is a second export channel with the same reach as cwd, and this gate
// no longer tries to parse it — it refuses anything not provably path-free.
//
// Two earlier designs tried to GATE the paths and both were defeated:
//
//	round 2: --add-dir modelled as arity-1 while the binary declares
//	         `<directories...>`, so extra directories went ungated.
//	round 3: `--add-dir --output-format <dir>` slipped through BOTH lists —
//	         variadic collection stopped at the "-", then --output-format's skip
//	         counter ate the directory. And a RELATIVE value was resolved against
//	         the ORCHESTRATOR's cwd while the child resolves it against its own,
//	         so gate and child could disagree about which directory it names.
func TestRefuseExtrasRejectsEveryPathBearingFlag(t *testing.T) {
	for _, extra := range [][]string{
		{"--add-dir", "D:/a"},
		{"--add-dir=D:/a"},
		{"--add-dir", "D:/a", "D:/b", "D:/c"},
		{"--mcp-config", "D:/a.json"},
		{"--settings", `{"permissions":{"additionalDirectories":["D:/x"]}}`},
		{"--plugin-dir", "D:/a"},
		{"--file", "id:rel/path"},
	} {
		if bad := RefuseExtras(extra); len(bad) == 0 {
			t.Errorf("%v must be refused: a path-bearing flag reaches the child verbatim and this gate cannot judge what it names", extra)
		}
	}
}

// Round 3's exact bypass: a variadic collector left open across a skip-counted
// flag, so the directory was neither gated nor reported.
func TestRefuseExtrasCatchesTheVariadicSkipCounterHole(t *testing.T) {
	bad := RefuseExtras([]string{"--add-dir", "--output-format", "D:/client-secret"})
	if len(bad) == 0 {
		t.Fatal("the round-3 hole must be closed: --add-dir followed by a skip-counted flag left the directory unreported")
	}
}

// Round 3's other bypass: a RELATIVE value the gate and the child resolve
// against different base directories.
func TestRefuseExtrasRejectsRelativeValues(t *testing.T) {
	if bad := RefuseExtras([]string{"--add-dir", "../client-secret"}); len(bad) == 0 {
		t.Fatal("a relative --extra value must be refused: the gate resolves it against the orchestrator's cwd, the child against its own")
	}
}

// Harmless flags still work, or the lane becomes unusable for legitimate calls.
func TestRefuseExtrasAllowsPathFreeFlags(t *testing.T) {
	for _, extra := range [][]string{
		{"--dangerously-skip-permissions"},
		{"--verbose"},
		{"--output-format", "json"},
		{"--model=glm-5.2"},
		{"--effort", "high", "--verbose"},
		{"--debug", "api"}, // optional-value flag, value consumed
		{"--debug"},        // …and without one
		{},
	} {
		if bad := RefuseExtras(extra); len(bad) != 0 {
			t.Errorf("%v carries no path and must be allowed, refused %v", extra, bad)
		}
	}
}

// Shapes that must never be mistaken for harmless.
func TestRefuseExtrasRefusesTheAmbiguous(t *testing.T) {
	for _, extra := range [][]string{
		{"bare-positional"},
		{"--", "anything", "after"},
		{"-"},
		{"--some-future-flag", "value"},
		// The skip counter must not swallow a following FLAG: if it did, the
		// --add-dir here would never be judged. (A dangling `--model` with no
		// value is deliberately NOT refused — it carries no path, and the child
		// rejects it on its own.)
		{"--model", "--add-dir", "D:/a"},
	} {
		if bad := RefuseExtras(extra); len(bad) == 0 {
			t.Errorf("%v must be refused", extra)
		}
	}
}

// A RELATIVE allowlist entry would resolve against whatever directory the
// orchestrator is standing in — "." self-allowlists every checkout in turn.
func TestRelativeAllowlistEntryIsRefused(t *testing.T) {
	d := Check("glm", absTest("dev", "anything"), Options{AllowRepos: []string{"."}})
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

// -d/--debug takes an OPTIONAL value, so the token after it is ambiguous. An inert
// filter word is fine; a path is not — `-d ../client-secret` previously exited 0
// with the path forwarded as an unjudged positional (review round 4).
func TestRefuseExtrasJudgesOptionalValues(t *testing.T) {
	for _, extra := range [][]string{
		{"-d", "../client-secret"},
		{"--debug", "D:/client"},
		{"--debug=../x"},
		{"-d", "./rel"},
	} {
		if bad := RefuseExtras(extra); len(bad) == 0 {
			t.Errorf("%v carries a path-shaped optional value and must be refused", extra)
		}
	}
	for _, extra := range [][]string{
		{"-d"}, {"--debug"}, {"--debug", "api"}, {"--debug=api"},
	} {
		if bad := RefuseExtras(extra); len(bad) != 0 {
			t.Errorf("%v is an inert debug filter and must be allowed, refused %v", extra, bad)
		}
	}
}

// absTest must actually produce a fully-qualified path on whatever platform the
// tests run on — otherwise the fixtures silently exercise the refusal branch and
// every "allowed" assertion above becomes vacuous. This is the guard that would
// have caught the Windows-shaped literals before CI did.
func TestAbsTestIsFullyQualifiedOnThisPlatform(t *testing.T) {
	p := absTest("Dev", "example")
	if !fullyQualified(p) {
		t.Fatalf("absTest produced %q, which this platform does not consider fully qualified", p)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("absTest produced %q, which is not absolute", p)
	}
}
