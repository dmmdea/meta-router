package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEmbedder serves an OpenAI-compatible /v1/embeddings endpoint returning
// deterministic 2-dim vectors.
func fakeEmbedder(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i := range req.Input {
			out.Data = append(out.Data, item{Index: i, Embedding: []float64{0.1, float64(i)}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func buildMRIndex(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mr-index-test.exe")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func writeSkillDir(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		dir := filepath.Join(root, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nname: " + n + "\ndescription: does " + n + " things for testing\n---\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readRefreshLog(t *testing.T, dir string) []refreshStatus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "refresh.log"))
	if err != nil {
		t.Fatalf("refresh.log missing: %v", err)
	}
	var out []refreshStatus
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var st refreshStatus
		if err := json.Unmarshal([]byte(line), &st); err != nil {
			t.Fatalf("bad refresh.log line %q: %v", line, err)
		}
		out = append(out, st)
	}
	return out
}

// TestRefreshE2E drives the built binary through the full MR-6 story:
// fresh build via refresh, a benign refresh, a guarded mass-removal refusal,
// and a --force override — asserting the refresh.log trail at each step.
func TestRefreshE2E(t *testing.T) {
	bin := buildMRIndex(t)
	srv := fakeEmbedder(t)
	work := t.TempDir()
	// The root's basename must be "skills" so IDs are bare invocable names.
	root := filepath.Join(work, "skills")
	writeSkillDir(t, root, "alpha", "beta", "gamma", "delta")
	outDir := filepath.Join(work, "meta")
	outPath := filepath.Join(outDir, "index.json")

	run := func(wantOK bool, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		out, err := cmd.CombinedOutput()
		if wantOK && err != nil {
			t.Fatalf("mr-index %v failed: %v\n%s", args, err, out)
		}
		if !wantOK && err == nil {
			t.Fatalf("mr-index %v unexpectedly succeeded:\n%s", args, out)
		}
		return string(out)
	}

	// 1. no index yet → refresh builds fresh and logs ok
	run(true, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath)
	log := readRefreshLog(t, outDir)
	if len(log) != 1 || !log[0].OK || log[0].EntriesAfter != 4 || log[0].Added != 4 {
		t.Fatalf("fresh-build status wrong: %+v", log)
	}

	// 2. removing 1 of 4 (25%) is under the 30% guard → allowed
	if err := os.RemoveAll(filepath.Join(root, "delta")); err != nil {
		t.Fatal(err)
	}
	run(true, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath)
	log = readRefreshLog(t, outDir)
	if len(log) != 2 || !log[1].OK || log[1].Removed != 1 || log[1].EntriesAfter != 3 {
		t.Fatalf("benign refresh status wrong: %+v", log[1])
	}

	// 3. removing 2 of 3 (67%) must be refused without --force
	os.RemoveAll(filepath.Join(root, "beta"))
	os.RemoveAll(filepath.Join(root, "gamma"))
	out := run(false, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath)
	if !strings.Contains(out, "beta") || !strings.Contains(out, "gamma") {
		t.Fatalf("refusal must print what it WOULD remove:\n%s", out)
	}
	log = readRefreshLog(t, outDir)
	if len(log) != 3 || log[2].OK {
		t.Fatalf("refusal must be logged as a failure: %+v", log[2])
	}
	// index untouched by the refusal
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if c := strings.Count(string(data), `"hash"`); c != 3 {
		t.Fatalf("refused refresh must leave the 3-entry index intact, found %d entries", c)
	}

	// 4. --force applies the mass removal and logs it as forced
	run(true, "refresh", "-skill-roots", root, "-endpoint", srv.URL, "-out", outPath, "-force")
	log = readRefreshLog(t, outDir)
	last := log[len(log)-1]
	if !last.OK || !last.Forced || last.EntriesAfter != 1 || last.Removed != 2 {
		t.Fatalf("forced refresh status wrong: %+v", last)
	}

	// 5. MR-16: each index-replacing refresh keeps exactly ONE dated .bak
	// of the previous index (older/hand-made baks pruned).
	baks, _ := filepath.Glob(outPath + ".bak*")
	if len(baks) != 1 {
		t.Fatalf("want exactly one .bak after refreshes, got %v", baks)
	}
	bakData, err := os.ReadFile(baks[0])
	if err != nil {
		t.Fatal(err)
	}
	// The backup holds the PRE-force index (3 entries), not the new one.
	if c := strings.Count(string(bakData), `"hash"`); c != 3 {
		t.Fatalf("backup should hold the previous 3-entry index, found %d entries", c)
	}
}

// TestRootsConfigE2E verifies D1(c): with no -skill-roots, mr-index resolves
// roots via roots.json next to the index — creating it on first use — so the
// no-flags SessionStart refresh sees the recorded set.
func TestRootsConfigE2E(t *testing.T) {
	bin := buildMRIndex(t)
	srv := fakeEmbedder(t)
	work := t.TempDir()
	outDir := filepath.Join(work, "meta")
	outPath := filepath.Join(outDir, "index.json")
	root := filepath.Join(work, "skills")
	writeSkillDir(t, root, "alpha")

	// Hand-write roots.json pointing at our fixture root; refresh (no
	// -skill-roots) must read it instead of discovering ~/.claude.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootsJSON := `{"version":1,"roots":[{"path":` + strconv(root) + `,"pack":"skills"}]}`
	if err := os.WriteFile(filepath.Join(outDir, "roots.json"), []byte(rootsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "refresh", "-endpoint", srv.URL, "-out", outPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("refresh via roots.json failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"alpha"`) {
		t.Fatalf("index should contain alpha from the roots.json root:\n%s", data)
	}
}

// strconv JSON-quotes a string (Windows paths need escaping).
func strconv(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
