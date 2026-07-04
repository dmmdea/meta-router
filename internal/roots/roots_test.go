package roots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
)

func writeSkill(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: test skill " + name + "\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildHome creates a fake ~/.claude with a user skills dir and a plugin
// cache in the marketplace layout (two versions of plug1) plus a
// direct-layout plugin (plug2), plus junk that must be ignored.
func buildHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills", "user-a"), "user-a")

	cache := filepath.Join(home, "plugins", "cache")
	v1 := filepath.Join(cache, "mkt", "plug1", "1.0.0", "skills", "s1")
	v2 := filepath.Join(cache, "mkt", "plug1", "2.0.0", "skills", "s1")
	writeSkill(t, v1, "s1")
	writeSkill(t, v2, "s1")
	writeSkill(t, filepath.Join(cache, "plug2", "skills", "s2"), "s2")
	// junk: temp scratch clone + a skill-less (MCP-only) plugin
	writeSkill(t, filepath.Join(cache, "temp_git_123_zz", "skills", "junk"), "junk")
	if err := os.MkdirAll(filepath.Join(cache, "mkt", "mcp-only", "1.0.0", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	// make 2.0.0 clearly newer for the mtime-based fallback
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(cache, "mkt", "plug1", "1.0.0"), old, old); err != nil {
		t.Fatal(err)
	}
	return home
}

func packsOf(rs []catalog.Root) map[string]string {
	m := map[string]string{}
	for _, r := range rs {
		m[r.Pack] = r.Path
	}
	return m
}

func TestDiscover_ManifestWins(t *testing.T) {
	home := buildHome(t)
	// Manifest pins plug1 to 1.0.0 even though 2.0.0 is newer on disk.
	manifest := map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"plug1@mkt": []map[string]string{
				{"installPath": filepath.Join(home, "plugins", "cache", "mkt", "plug1", "1.0.0")},
			},
			"mcp-only@mkt": []map[string]string{
				{"installPath": filepath.Join(home, "plugins", "cache", "mkt", "mcp-only", "1.0.0")},
			},
		},
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(home, "plugins", "installed_plugins.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := Discover(home)
	packs := packsOf(got)
	if _, ok := packs[catalog.UserPack]; !ok {
		t.Fatalf("user skills root missing: %+v", got)
	}
	p1, ok := packs["plug1"]
	if !ok {
		t.Fatalf("plug1 root missing: %+v", got)
	}
	if want := filepath.Join(home, "plugins", "cache", "mkt", "plug1", "1.0.0", "skills"); p1 != want {
		t.Fatalf("manifest must pin the active version:\n got %s\nwant %s", p1, want)
	}
	if _, ok := packs["mcp-only"]; ok {
		t.Fatalf("skill-less plugin must not become a root: %+v", got)
	}
	// user root must come first (stable, deterministic order)
	if got[0].Pack != catalog.UserPack {
		t.Fatalf("user root must be first, got %+v", got)
	}
}

func TestDiscover_CacheScanFallback(t *testing.T) {
	home := buildHome(t) // no manifest written
	got := Discover(home)
	packs := packsOf(got)
	if want := filepath.Join(home, "plugins", "cache", "mkt", "plug1", "2.0.0", "skills"); packs["plug1"] != want {
		t.Fatalf("fallback must pick the newest version with skills:\n got %s\nwant %s", packs["plug1"], want)
	}
	if want := filepath.Join(home, "plugins", "cache", "plug2", "skills"); packs["plug2"] != want {
		t.Fatalf("direct-layout plugin root wrong:\n got %s\nwant %s", packs["plug2"], want)
	}
	for pack := range packs {
		if pack != catalog.UserPack && pack != "plug1" && pack != "plug2" {
			t.Fatalf("unexpected pack %q (temp/junk must be skipped): %+v", pack, got)
		}
	}
}

func TestSaveLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roots.json")
	in := []catalog.Root{
		{Path: filepath.Join(dir, "skills"), Pack: catalog.UserPack},
		{Path: filepath.Join(dir, "plug", "skills"), Pack: "plug"},
	}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("roundtrip mismatch:\n in %+v\nout %+v", in, out)
	}
}

func TestLoad_MissingAndInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "roots.json")); !os.IsNotExist(err) {
		t.Fatalf("missing file must return not-exist, got %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("invalid file must error so callers can rediscover")
	}
}

func TestConfigPathFor(t *testing.T) {
	got := ConfigPathFor(filepath.Join("x", "y", "index.json"))
	if got != filepath.Join("x", "y", "roots.json") {
		t.Fatalf("roots.json must sit next to the index: %s", got)
	}
}
