package codexlane

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setHome points os.UserHomeDir at dir on every OS (USERPROFILE on Windows,
// HOME elsewhere) — S2R-13 portability: the WSL -race gate runs these too.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
}

func TestEnsureHomeSeedsAuthAndConfig(t *testing.T) {
	fakeUser := t.TempDir()
	setHome(t, fakeUser)
	if err := os.MkdirAll(filepath.Join(fakeUser, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeUser, ".codex", "auth.json"), []byte(`{"tokens":"SEED"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	home, cleanup, err := EnsureHome(base)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(home, "auth.json")); err != nil || string(b) != `{"tokens":"SEED"}` {
		t.Fatalf("auth must be seeded from the operator's login (R12): %v %s", err, b)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || !strings.Contains(string(cfg), `cli_auth_credentials_store = "file"`) {
		t.Fatalf("config.toml must force file-backed credentials (fact refresh §3): %v %s", err, cfg)
	}
	if runtime.GOOS == "windows" {
		// CLI >=0.153 rejects every command on Windows without a native
		// sandbox mode in the ACTIVE home (live 2026-09-06).
		if !strings.Contains(string(cfg), "[windows]") || !strings.Contains(string(cfg), `sandbox = "elevated"`) {
			t.Fatalf("config.toml must seed the native Windows sandbox mode: %s", cfg)
		}
	}
	cleanup()
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the per-run home: %v", err)
	}
}

func TestEnsureHomeFailsLoudWithoutLogin(t *testing.T) {
	setHome(t, t.TempDir()) // no ~/.codex/auth.json
	if _, _, err := EnsureHome(t.TempDir()); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("missing auth must name the fix, got %v", err)
	}
}

func TestVersionGateComparator(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"codex-cli 0.142.5", true}, {"OpenAI Codex v0.143.0", true},
		{"codex-cli 0.142.3", false}, {"0.99.9", false}, {"garbage", false},
		{"codex-cli 1.0.0", true},
	} {
		if got := versionAtLeast(tc.raw, 0, 142, 5); got != tc.ok {
			t.Fatalf("versionAtLeast(%q) = %v, want %v", tc.raw, got, tc.ok)
		}
	}
}

// The Windows sandbox mode is validated before a home is written: an unknown
// value would be seeded verbatim and leave the CLI in its reject-everything
// posture, indistinguishable from a working lane at the receipt level.
func TestEnsureHomeWithValidatesWindowsSandbox(t *testing.T) {
	fakeUser := t.TempDir()
	setHome(t, fakeUser)
	if err := os.MkdirAll(filepath.Join(fakeUser, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeUser, ".codex", "auth.json"), []byte(`{"tokens":"SEED"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if _, _, err := EnsureHomeWith(t.TempDir(), "disabled"); err == nil || !strings.Contains(err.Error(), "codex_windows_sandbox") {
			t.Fatalf("an unknown mode must be refused naming the config field: %v", err)
		}
	}
	home, cleanup, err := EnsureHomeWith(t.TempDir(), WindowsSandboxUnelev)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if runtime.GOOS == "windows" && !strings.Contains(string(cfg), `sandbox = "unelevated"`) {
		t.Fatalf("the fallback mode must be seeded verbatim: %s", cfg)
	}
	if runtime.GOOS != "windows" && strings.Contains(string(cfg), "[windows]") {
		t.Fatalf("no Windows table off Windows: %s", cfg)
	}
	if !ValidWindowsSandbox("elevated") || !ValidWindowsSandbox("unelevated") || ValidWindowsSandbox("danger-full-access") || ValidWindowsSandbox("") {
		t.Fatal("ValidWindowsSandbox must accept exactly the two documented modes")
	}
}
