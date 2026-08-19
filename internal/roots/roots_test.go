package roots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// Stale is what stops the silent index decay: `refresh` reads roots.json and
// never re-discovers, so when a plugin updates and its VERSIONED cache path
// disappears, every skill in that pack drops out of the index with no
// diagnostic. Observed in production 2026-08-10: 7 of 13 roots dead, index
// 155 -> 112 entries, the whole superpowers pack gone. The removal guard never
// fired because each decay step was under its 30% threshold.
//
// The partition mirrors resolveRoots' ownership rule exactly: discovery owns
// the ~/.claude skills space (a vanished pack there SHOULD drop), the operator
// owns flat roots and anything outside claudeDir (dropping those would destroy
// hand edits).
func TestStale_PartitionsByOwnership(t *testing.T) {
	claude := t.TempDir()
	outside := t.TempDir()

	live := filepath.Join(claude, "skills")
	writeSkill(t, filepath.Join(live, "a"), "a")

	rs := []catalog.Root{
		{Path: live, Pack: catalog.UserPack},
		{Path: filepath.Join(claude, "plugins", "cache", "sp", "6.1.1", "skills"), Pack: "superpowers"},
		{Path: filepath.Join(outside, "gone"), Pack: "handadded"},
		{Path: filepath.Join(claude, "commands"), Pack: catalog.UserPack, Kind: catalog.KindCommands},
	}

	discovery, operator, unknown := Stale(rs, claude)
	if len(unknown) != 0 {
		t.Fatalf("provably-absent roots are not unknown: %+v", unknown)
	}

	if len(discovery) != 1 || discovery[0].Pack != "superpowers" {
		t.Fatalf("a vanished plugin root under claudeDir is discovery-owned; got %+v", discovery)
	}
	// A dead flat root and a dead outside root are BOTH operator-owned: only a
	// human puts them in roots.json, so rediscovery must never silently drop
	// them.
	if len(operator) != 2 {
		t.Fatalf("dead flat + outside roots are operator-owned; got %+v", operator)
	}
	for _, r := range operator {
		if r.Pack != "handadded" && r.Kind != catalog.KindCommands {
			t.Fatalf("unexpected operator-owned entry %+v", r)
		}
	}
}

// A live root must never be reported stale — otherwise refresh rediscovers on
// every run and the ownership rule stops meaning anything.
func TestStale_LiveRootsAreNotStale(t *testing.T) {
	claude := t.TempDir()
	live := filepath.Join(claude, "skills")
	writeSkill(t, filepath.Join(live, "a"), "a")

	discovery, operator, unknown := Stale([]catalog.Root{{Path: live, Pack: catalog.UserPack}}, claude)
	if len(discovery) != 0 || len(operator) != 0 || len(unknown) != 0 {
		t.Fatalf("live root reported stale: %+v %+v %+v", discovery, operator, unknown)
	}
}

// A path that EXISTS but is not a directory is not proof the pack was
// uninstalled, and must never force rediscovery — rediscovery ends in Save,
// which would delete the root permanently.
func TestStale_NonDirectoryIsUnknownNotGone(t *testing.T) {
	claude := t.TempDir()
	f := filepath.Join(claude, "plugins", "notadir")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	discovery, operator, unknown := Stale([]catalog.Root{{Path: f, Pack: "weird"}}, claude)
	if len(discovery) != 0 {
		t.Fatalf("an existing path must not be classified as an uninstall: %+v", discovery)
	}
	if len(operator) != 0 {
		t.Fatalf("not operator-owned either: %+v", operator)
	}
	if len(unknown) != 1 || unknown[0].Err == nil {
		t.Fatalf("must be reported as unknown WITH the reason: %+v", unknown)
	}
}

// The polarity here is unsafe: judging a claudeDir root "outside" means the
// decay is never repaired. Plugin paths reach roots.json verbatim from
// installed_plugins.json, written by another program, so casing is not
// guaranteed to match os.UserHomeDir on Windows.
func TestStale_WindowsCasingStillClassifiesAsDiscoveryOwned(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive containment is a Windows concern")
	}
	claude := t.TempDir()
	dead := filepath.Join(strings.ToUpper(claude), "plugins", "cache", "sp", "6.1.1", "skills")

	discovery, operator, _ := Stale([]catalog.Root{{Path: dead, Pack: "superpowers"}}, claude)
	if len(discovery) != 1 {
		t.Fatalf("an upper-cased claudeDir path is still inside the tree: discovery=%+v operator=%+v", discovery, operator)
	}
}

// The production failure had a dead path that is a PREFIX-sibling of claudeDir
// only by string comparison. Ownership must be decided on real path boundaries,
// not raw HasPrefix: "<claude>x/skills" is OUTSIDE "<claude>".
func TestStale_OwnershipUsesPathBoundaryNotStringPrefix(t *testing.T) {
	claude := t.TempDir()
	sibling := claude + "x"

	discovery, operator, _ := Stale([]catalog.Root{
		{Path: filepath.Join(sibling, "skills"), Pack: "sibling"},
	}, claude)

	if len(discovery) != 0 {
		t.Fatalf("a sibling dir that merely shares a string prefix is NOT under claudeDir: %+v", discovery)
	}
	if len(operator) != 1 {
		t.Fatalf("it is operator-owned and must be preserved: %+v", operator)
	}
}

// The reserved-namespace guard: a skills-class root whose pack name would
// mint agent:/tool: IDs is refused loudly, and flat roots (whose IDs never
// derive from Pack) stay accepted.
func TestLoadRefusesReservedPackOnSkillsRoot(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "roots.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := write(`{"version":1,"roots":[{"path":"C:/x","pack":"tool"}]}`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "reserved ID namespace") {
		t.Fatalf("skills-class pack \"tool\" must be refused, got err=%v", err)
	}
	p = write(`{"version":1,"roots":[{"path":"C:/x","pack":"agent","kind":"skills"}]}`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "reserved ID namespace") {
		t.Fatalf("explicit-skills pack \"agent\" must be refused, got err=%v", err)
	}
	p = write(`{"version":1,"roots":[{"path":"C:/x","pack":"tool","kind":"tools"}]}`)
	if _, err := Load(p); err != nil {
		t.Fatalf("a flat tools root may use any pack (IDs don't derive from Pack): %v", err)
	}
}

// KindTools is accepted by Load, and the unknown-kind error names it among
// the valid kinds (an operator who typos "tool" must be able to discover
// "tools" from the message).
func TestLoadKindToolsAcceptedAndAdvertised(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "roots.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"roots":[{"path":"C:/x","pack":"local-offload","kind":"tools"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := Load(p)
	if err != nil || len(rs) != 1 || rs[0].Kind != catalog.KindTools {
		t.Fatalf("kind tools not loaded: rs=%+v err=%v", rs, err)
	}
	if err := os.WriteFile(p, []byte(`{"version":1,"roots":[{"path":"C:/x","pack":"p","kind":"tool"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(p)
	if err == nil || !strings.Contains(err.Error(), `"tools"`) {
		t.Fatalf("unknown-kind error must advertise \"tools\", got: %v", err)
	}
}
