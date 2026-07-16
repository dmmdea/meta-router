package retrievers

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// homeTo points os.UserHomeDir at a temp dir on every platform (Windows reads
// USERPROFILE, unix reads HOME) so the machine-local endpoints file can be
// exercised without touching the real ~/.meta-router.
func homeTo(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
}

func writeEndpointsFile(t *testing.T, home string, body string) {
	t.Helper()
	dir := filepath.Join(home, ".meta-router")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, endpointsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The -endpoint flag is authoritative: an operator who pins one endpoint gets
// that endpoint or an error — never a silent fallback to a different service.
// This is what keeps TestDeadEndpointFailsFast honest.
func TestResolveEndpoints_FlagIsAuthoritative(t *testing.T) {
	home := t.TempDir()
	homeTo(t, home)
	writeEndpointsFile(t, home, `["http://file:1"]`)
	t.Setenv(EndpointEnv, "http://env:1")

	got := ResolveEndpoints("http://flag:1")
	want := []string{"http://flag:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flag must win and must NOT be padded with fallbacks: got %v want %v", got, want)
	}
}

func TestResolveEndpoints_EnvBeatsFileAndDefaults(t *testing.T) {
	home := t.TempDir()
	homeTo(t, home)
	writeEndpointsFile(t, home, `["http://file:1"]`)
	t.Setenv(EndpointEnv, "http://env:1,http://env:2")

	got := ResolveEndpoints("")
	want := []string{"http://env:1", "http://env:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestResolveEndpoints_FileBeatsDefaults(t *testing.T) {
	home := t.TempDir()
	homeTo(t, home)
	os.Unsetenv(EndpointEnv)
	writeEndpointsFile(t, home, `["http://file:1", "http://file:2"]`)

	got := ResolveEndpoints("")
	want := []string{"http://file:1", "http://file:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// The whole point of the PC-agnostic default: with nothing configured anywhere,
// both known local embedder ports are tried in order, so the same binary and the
// same shared settings.json work on a host serving :11436 (llama-swap) and on a
// host serving :18793 (embedder sidecar).
func TestResolveEndpoints_DefaultsWhenNothingConfigured(t *testing.T) {
	home := t.TempDir()
	homeTo(t, home)
	os.Unsetenv(EndpointEnv)

	got := ResolveEndpoints("")
	want := DefaultEndpoints()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if len(got) < 2 {
		t.Fatalf("default chain must contain a fallback candidate, got %v", got)
	}
}

// A malformed or unreadable endpoints.json must degrade to the defaults, never
// leave the surfacer with no endpoint at all.
func TestResolveEndpoints_MalformedFileFallsBackToDefaults(t *testing.T) {
	home := t.TempDir()
	homeTo(t, home)
	os.Unsetenv(EndpointEnv)
	writeEndpointsFile(t, home, `{ this is not valid json `)

	got := ResolveEndpoints("")
	if !reflect.DeepEqual(got, DefaultEndpoints()) {
		t.Fatalf("malformed file must fall back to defaults, got %v", got)
	}
}

func TestSplitEndpoints_TrimsDedupesAndStripsTrailingSlash(t *testing.T) {
	got := splitEndpoints("  http://a:1/ , http://b:2 ,, http://a:1 , http://b:2/ ")
	want := []string{"http://a:1", "http://b:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if len(splitEndpoints("   ,  , ")) != 0 {
		t.Fatal("a spec of only separators must yield no endpoints")
	}
}
