package egress

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeDirLink creates a directory link at link pointing to target, using the
// mechanism an UNPRIVILEGED user actually has.
//
// On Windows that is a junction (`mklink /J`) — deliberately not `mklink /D`,
// which needs elevation. That asymmetry is the whole point of the tests using
// this helper: the guard we shipped resolved only real symlinks, i.e. only the
// link type that requires admin, and missed the one any user, installer or build
// script creates freely. Returns false when no mechanism is available so the
// caller can skip rather than fail for the wrong reason.
func makeDirLink(t *testing.T, link, target string) bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Logf("mklink /J unavailable: %v\n%s", err, out)
			return false
		}
		return true
	}
	if err := os.Symlink(target, link); err != nil {
		t.Logf("symlink unavailable: %v", err)
		return false
	}
	return true
}

// canonical must see through the link, or every containment test above it is
// comparing spellings rather than locations.
func TestCanonicalResolvesADirLink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if !makeDirLink(t, link, target) {
		t.Skip("no unprivileged directory-link mechanism on this platform")
	}
	got, err := canonical(link)
	if err != nil {
		t.Fatalf("canonical(%s): %v", link, err)
	}
	want, err := canonical(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonical did not resolve the link: got %q want %q", got, want)
	}
	// The lexical predecessor of this check returned the link path unchanged,
	// which is exactly how a junction escaped an allowlisted repo.
	if got == filepath.Clean(link) {
		t.Fatalf("canonical returned the link path itself (%q) — resolution did not happen", got)
	}
}

// A path that does not exist yet cannot be a reparse point, but an ANCESTOR can.
func TestCanonicalResolvesAnAncestorLinkForAMissingLeaf(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if !makeDirLink(t, link, target) {
		t.Skip("no unprivileged directory-link mechanism on this platform")
	}
	got, err := canonical(filepath.Join(link, "not", "created", "yet"))
	if err != nil {
		t.Fatal(err)
	}
	realBase, _ := canonical(target)
	want := filepath.Join(realBase, "not", "created", "yet")
	if got != want {
		t.Fatalf("an ancestor link must still resolve: got %q want %q", got, want)
	}
}

// A fabricated path (several tests above use them, and a dispatch can name a
// directory that does not exist yet) must stay usable: no error, and the same
// location as the lexical form.
//
// Asserted case-insensitively on purpose. The first version of this test
// demanded byte equality with filepath.Clean and failed on the review machine
// because an existing ANCESTOR resolved to its true on-disk casing
// (D:/dev/... -> D:\Dev\...). That is correct behaviour — canonicalization is
// supposed to fix the spelling — and a test that hard-codes one machine's
// directory casing is the environment assumption that has bitten this repo
// twice already.
func TestCanonicalOnAFabricatedPathStaysUsable(t *testing.T) {
	for _, p := range []string{
		filepath.FromSlash("D:/dev/pepsdubai/does-not-exist-anywhere"),
		filepath.Join(t.TempDir(), "no", "such", "tree"),
	} {
		got, err := canonical(p)
		if err != nil {
			t.Fatalf("a non-existent path must not error: %v", err)
		}
		if !strings.EqualFold(got, filepath.Clean(p)) {
			t.Fatalf("canonical(%q) must denote the same location, got %q", p, got)
		}
	}
}
