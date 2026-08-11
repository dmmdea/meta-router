package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/hostcfg"
)

// A hand-maintained settings.json: deliberate key order, a comment-ish note,
// an existing SessionStart hook that belongs to the operator, and a statusline
// the tee has to wrap rather than replace.
const opSettings = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "theirs.exe" }
        ]
      }
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "node theirs-statusline.js"
  },
  "_note": "hand-written, order matters"
}
`

type hostFixture struct {
	home, bin, state string
	settings         string
	original         []byte
}

func newHostFixture(t *testing.T, settingsBody string) *hostFixture {
	t.Helper()
	home := t.TempDir()
	// The install's state lives under the home being wired, so the fixture must
	// look there too — MR_ORCH_STATE deliberately does NOT redirect it. That is
	// what makes -home real isolation instead of the half-isolation that let a
	// scratch install plant a manifest in the operator's own state dir.
	h := &hostFixture{home: home, bin: t.TempDir(), state: hostStateDir(home)}
	if err := os.MkdirAll(h.state, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // any install writing HERE is a bug
	t.Setenv("MR_TEST_HOST_STUB", "1")
	t.Setenv("MR_TEST_STUB_LOG", filepath.Join(h.state, "stub.log"))
	t.Setenv("PATH", hostStubOnPATH(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.settings = filepath.Join(h.home, ".claude", "settings.json")
	if settingsBody != "" {
		if err := os.MkdirAll(filepath.Dir(h.settings), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.settings, []byte(settingsBody), 0o644); err != nil {
			t.Fatal(err)
		}
		h.original = []byte(settingsBody)
	}
	return h
}

func (h *hostFixture) args(extra ...string) []string {
	return append([]string{"claude", "-home", h.home, "-bin", h.bin, "-json"}, extra...)
}

func (h *hostFixture) read(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *hostFixture) manifestPath() string {
	return hostcfg.ManifestPath(h.state, "claude")
}

func (h *hostFixture) stubLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.state, "stub.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

// capture runs fn with stdout redirected to a FILE (never a pipe: an unread
// pipe deadlocks once the report outgrows the buffer).
func capture(t *testing.T, fn func() error) (installReport, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "report-*.json")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	runErr := fn()
	os.Stdout = old
	f.Close()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var rep installReport
	if len(b) > 0 {
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatalf("report is not JSON: %v\n%s", err, b)
		}
	}
	return rep, runErr
}

func statusOf(rep installReport, needle string) string {
	for _, s := range rep.Steps {
		if strings.Contains(s.Desc, needle) {
			return s.Status
		}
	}
	return "<no such step>"
}

// The charter's acceptance gate, first half: it installs on a scratch HOME.
func TestInstallOnScratchHome(t *testing.T) {
	h := newHostFixture(t, opSettings)
	rep, err := capture(t, func() error { return runInstall(h.args()) })
	if err != nil {
		t.Fatalf("install: %v (%+v)", err, rep)
	}
	if rep.Result != "ok" {
		t.Fatalf("result %q: %+v", rep.Result, rep.Steps)
	}
	got := h.read(t)

	hookBin := filepath.Join(h.bin, exeName("mr-hook"))
	indexBin := filepath.Join(h.bin, exeName("mr-index"))
	for _, want := range []string{jsonEsc(hookBin), jsonEsc(indexBin), teeFileName} {
		if !strings.Contains(got, want) {
			t.Fatalf("install did not wire %q:\n%s", want, got)
		}
	}
	// The operator's own content survives, in place.
	for _, keep := range []string{`"theirs.exe"`, `"_note": "hand-written, order matters"`, `"model": "opus"`} {
		if !strings.Contains(got, keep) {
			t.Fatalf("install disturbed operator content %q:\n%s", keep, got)
		}
	}
	if strings.Index(got, `"model"`) > strings.Index(got, `"hooks"`) {
		t.Fatalf("install reordered the document:\n%s", got)
	}
	// The tee is written from the embedded copy, and told what it wraps.
	tee, err := os.ReadFile(filepath.Join(h.bin, teeFileName))
	if err != nil {
		t.Fatalf("tee script not installed: %v", err)
	}
	if string(tee) != string(statuslineTeeJS) {
		t.Fatal("installed tee does not match the embedded copy")
	}
	var delegate struct {
		Command string `json:"command"`
	}
	db, err := os.ReadFile(filepath.Join(h.state, "statusline-delegate.json"))
	if err != nil {
		t.Fatalf("delegate not recorded: %v", err)
	}
	if err := json.Unmarshal(db, &delegate); err != nil {
		t.Fatal(err)
	}
	if delegate.Command != "node theirs-statusline.js" {
		t.Fatalf("the tee must delegate to the statusline it replaced, got %q", delegate.Command)
	}
	// The MCP registry was written by the HOST's cli, not by us.
	if log := h.stubLog(t); !strings.Contains(log, "claude mcp add --scope user meta-router") {
		t.Fatalf("host cli was not used to register the server: %q", log)
	}
	p, _, err := probeClaudeMCP(filepath.Join(h.home, ".claude.json"))
	if err != nil || p != hostcfg.PresentFile {
		t.Fatalf("server not registered: presence=%v err=%v", p, err)
	}
	if _, _, err := hostcfg.LoadManifest(h.manifestPath()); err != nil {
		t.Fatalf("manifest: %v", err)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	after := h.read(t)
	rep, err := capture(t, func() error { return runInstall(h.args()) })
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if h.read(t) != after {
		t.Fatal("a second install changed the config; it must be a no-op")
	}
	for _, s := range rep.Steps {
		if s.Status != stManaged {
			t.Fatalf("step %q reported %q, want %q", s.Desc, s.Status, stManaged)
		}
	}
	if !strings.Contains(rep.Note, "nothing left to wire") {
		t.Fatalf("note should say there is nothing left to wire: %q", rep.Note)
	}
}

// The charter's acceptance gate, third half: uninstall restores byte-identical.
func TestUninstallRestoresByteIdentical(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	if h.read(t) == opSettings {
		t.Fatal("precondition: install must actually have changed the file")
	}
	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err != nil {
		t.Fatalf("uninstall: %v (%+v)", err, rep)
	}
	if rep.Restore != restoreByte {
		t.Fatalf("restore mode %q, want %q: %+v", rep.Restore, restoreByte, rep.Steps)
	}
	if got := h.read(t); got != opSettings {
		t.Fatalf("uninstall did not restore the file byte for byte.\n--- want ---\n%s\n--- got ---\n%s", opSettings, got)
	}
	if _, err := os.Stat(filepath.Join(h.bin, teeFileName)); !os.IsNotExist(err) {
		t.Fatalf("the tee script this install created must be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.state, "statusline-delegate.json")); !os.IsNotExist(err) {
		t.Fatalf("the delegate file this install created must be removed: %v", err)
	}
	if _, err := os.Stat(h.manifestPath()); !os.IsNotExist(err) {
		t.Fatalf("manifest must be gone after uninstall: %v", err)
	}
	p, _, err := probeClaudeMCP(filepath.Join(h.home, ".claude.json"))
	if err != nil || p != hostcfg.AbsentFile {
		t.Fatalf("server should have been deregistered: presence=%v err=%v", p, err)
	}
}

// The charter's acceptance gate, second half: it refuses on drift. Drift here is
// the dangerous direction — something ELSE now owns a key we installed, so
// putting our recorded value back would destroy an operator setting.
func TestUninstallRefusesOnDrift(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	// The operator replaces the statusline with their own.
	d, _, err := hostcfg.LoadDoc(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MemberSet([]string{"statusLine"},
		json.RawMessage(`{"type":"command","command":"node someone-elses.js"}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	drifted := h.read(t)

	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err == nil {
		t.Fatal("uninstall must refuse when a managed key now holds something else")
	}
	if rep.Result != "refused" {
		t.Fatalf("result %q, want refused", rep.Result)
	}
	if statusOf(rep, "statusLine") != stDrift {
		t.Fatalf("the statusLine step should report drift: %+v", rep.Steps)
	}
	if h.read(t) != drifted {
		t.Fatal("a refused uninstall must write NOTHING")
	}
	if _, err := os.Stat(h.manifestPath()); err != nil {
		t.Fatalf("a refused uninstall must keep the manifest: %v", err)
	}
}

// Two entries invoking our binary is a state no install of ours produces, so
// removing "the" one would be a coin flip.
func TestUninstallRefusesOnAmbiguousEntry(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	d, _, err := hostcfg.LoadDoc(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	path := []string{"hooks", "UserPromptSubmit"}
	dup, err := d.ArrayAt(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ArrayAppend(path, dup); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	before := h.read(t)
	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err == nil {
		t.Fatal("two entries invoking our binary must refuse, not remove one at random")
	}
	if statusOf(rep, "UserPromptSubmit") != stDrift {
		t.Fatalf("expected drift on the duplicated hook: %+v", rep.Steps)
	}
	if h.read(t) != before {
		t.Fatal("a refused uninstall must write NOTHING")
	}
}

// Ownership is recorded, never inferred: an entry that looks exactly like ours
// but predates any manifest belongs to whoever wrote it.
func TestInstallRefusesToAdoptUnmanagedEntry(t *testing.T) {
	h := newHostFixture(t, opSettings)
	hookBin := filepath.Join(h.bin, exeName("mr-hook"))
	d, _, err := hostcfg.LoadDoc(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ArrayAppend([]string{"hooks", "UserPromptSubmit"},
		json.RawMessage(`{"hooks":[{"type":"command","command":`+mustQuote(hookBin)+`}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	handWired := h.read(t)

	rep, err := capture(t, func() error { return runInstall(h.args()) })
	if err == nil {
		t.Fatal("install must refuse to adopt an entry it did not write")
	}
	if statusOf(rep, "UserPromptSubmit") != stConflict {
		t.Fatalf("expected a conflict on the hand-wired hook: %+v", rep.Steps)
	}
	if h.read(t) != handWired {
		t.Fatal("a refused install must write NOTHING")
	}
	if _, err := os.Stat(h.manifestPath()); !os.IsNotExist(err) {
		t.Fatalf("a refused install must not leave a manifest: %v", err)
	}
	if log := h.stubLog(t); log != "" {
		t.Fatalf("a refused install must not have touched the host registry: %q", log)
	}
}

// When the file changed for reasons of the operator's own, their work wins:
// we take our entries out and say plainly that this was not a byte restore.
func TestUninstallIsSurgicalWhenTheFileChanged(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	d, _, err := hostcfg.LoadDoc(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ArrayAppend([]string{"hooks", "SessionStart"},
		json.RawMessage(`{"hooks":[{"type":"command","command":"operator-added-later.exe"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}

	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err != nil {
		t.Fatalf("uninstall: %v (%+v)", err, rep)
	}
	if rep.Restore != restoreSurgical {
		t.Fatalf("restore mode %q, want %q", rep.Restore, restoreSurgical)
	}
	got := h.read(t)
	if !strings.Contains(got, "operator-added-later.exe") {
		t.Fatalf("surgical uninstall discarded the operator's later edit:\n%s", got)
	}
	if !strings.Contains(got, "theirs.exe") || !strings.Contains(got, `"_note"`) {
		t.Fatalf("surgical uninstall damaged untouched content:\n%s", got)
	}
	for _, gone := range []string{exeName("mr-hook"), exeName("mr-index"), teeFileName} {
		if strings.Contains(got, gone) {
			t.Fatalf("surgical uninstall left %q behind:\n%s", gone, got)
		}
	}
	// The array we created for our hook goes with it, rather than leaving an
	// empty list in someone's config.
	if strings.Contains(got, "UserPromptSubmit") {
		t.Fatalf("an array created solely for our hook must not survive as []:\n%s", got)
	}
	// The statusline we wrapped comes back.
	if !strings.Contains(got, "node theirs-statusline.js") {
		t.Fatalf("the wrapped statusline was not restored:\n%s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("surgical uninstall produced invalid JSON:\n%s", got)
	}
}

// The tee delegates to the statusline it replaces. With no statusline to wrap,
// installing it would replace the host's built-in default with a blank line, so
// the step is skipped and said so.
func TestTeeSkippedWhenThereIsNoStatuslineToWrap(t *testing.T) {
	h := newHostFixture(t, `{
  "model": "opus"
}
`)
	rep, err := capture(t, func() error { return runInstall(h.args()) })
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if statusOf(rep, "statusLine") != stSkip {
		t.Fatalf("expected the tee step to be skipped: %+v", rep.Steps)
	}
	if got := h.read(t); strings.Contains(got, "statusLine") {
		t.Fatalf("no statusLine should have been written:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(h.bin, teeFileName)); !os.IsNotExist(err) {
		t.Fatal("the tee script must not be installed when the tee step is skipped")
	}
	// The hooks still went in — a skipped tee is not a failed install.
	if statusOf(rep, "UserPromptSubmit") != stWire {
		t.Fatalf("hooks should still be wired: %+v", rep.Steps)
	}
}

// A dry run is what the operator runs against their real machine, so it must
// write nothing at all — not the config, not the manifest, not the registry.
func TestDryRunWritesNothing(t *testing.T) {
	h := newHostFixture(t, opSettings)
	rep, err := capture(t, func() error { return runInstall(h.args("-dry-run")) })
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !rep.DryRun || rep.Result != "ok" {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if h.read(t) != opSettings {
		t.Fatal("dry run modified the config")
	}
	if _, err := os.Stat(h.manifestPath()); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a manifest")
	}
	if log := h.stubLog(t); log != "" {
		t.Fatalf("dry run invoked the host cli: %q", log)
	}
	if _, err := os.Stat(filepath.Join(h.bin, teeFileName)); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the tee script")
	}
}

// The real Claude Code cli rewrites settings.json as a side effect of ANY
// invocation (verified 2026-08-11: `claude mcp list` normalised `model` and
// re-indented the file). The installer therefore must record what is on DISK
// when it finishes, not the bytes it wrote — otherwise the manifest's hash is
// stale the moment install returns and the byte-identical restore path is dead
// code on every real machine.
func TestManifestHashesWhatTheHostLeftBehind(t *testing.T) {
	h := newHostFixture(t, opSettings)
	t.Setenv("MR_TEST_STUB_REWRITE", "1")
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	man, _, err := hostcfg.LoadManifest(h.manifestPath())
	if err != nil {
		t.Fatal(err)
	}
	onDisk := hostcfg.SumHex([]byte(h.read(t)))
	var rec *hostcfg.FileRec
	for i := range man.Files {
		if man.Files[i].Path == h.settings {
			rec = &man.Files[i]
		}
	}
	if rec == nil {
		t.Fatal("settings.json is not recorded in the manifest")
	}
	if rec.SHA256After != onDisk {
		t.Fatalf("manifest records %q but the file on disk hashes %q — the host rewrote it after we did, and the manifest did not notice",
			rec.SHA256After, onDisk)
	}
	// And the consequence that actually matters: the restore is still byte-exact.
	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if rep.Restore != restoreByte {
		t.Fatalf("restore %q, want %q — a host rewrite during install must not cost the byte-identical path", rep.Restore, restoreByte)
	}
	if got := h.read(t); got != opSettings {
		t.Fatalf("uninstall did not restore byte for byte:\n%s", got)
	}
}

// The delegated registry step is the one most likely to fail on a real machine.
// When it does, the config must not be left half-wired.
func TestFailedHostCLIRollsBackEverything(t *testing.T) {
	h := newHostFixture(t, opSettings)
	t.Setenv("MR_TEST_STUB_FAIL", "1")
	rep, err := capture(t, func() error { return runInstall(h.args()) })
	if err == nil {
		t.Fatal("a failing host cli must fail the install")
	}
	_ = rep
	if got := h.read(t); got != opSettings {
		t.Fatalf("rollback did not restore the config byte for byte:\n%s", got)
	}
	if _, err := os.Stat(h.manifestPath()); !os.IsNotExist(err) {
		t.Fatal("a failed install must not leave a manifest claiming ownership")
	}
	if _, err := os.Stat(filepath.Join(h.bin, teeFileName)); !os.IsNotExist(err) {
		t.Fatal("a failed install must not leave the tee script behind")
	}
	if _, err := os.Stat(filepath.Join(h.state, "statusline-delegate.json")); !os.IsNotExist(err) {
		t.Fatal("a failed install must not leave the delegate file behind")
	}
}

// An unreadable config is not an empty one. Treating it as absent would build a
// fresh settings.json over a file we merely failed to open.
func TestUnreadableConfigIsAnError(t *testing.T) {
	h := newHostFixture(t, "")
	if err := os.MkdirAll(h.settings, 0o755); err != nil { // a DIRECTORY where the file goes
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err == nil {
		t.Fatal("an unreadable settings.json must abort the install")
	}
}

func TestUninstallWithNoManifestDoesNothing(t *testing.T) {
	h := newHostFixture(t, opSettings)
	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err != nil {
		t.Fatalf("uninstall with nothing installed should succeed quietly: %v", err)
	}
	if rep.Restore != restoreNone {
		t.Fatalf("restore %q, want %q", rep.Restore, restoreNone)
	}
	if h.read(t) != opSettings {
		t.Fatal("uninstall touched a config it never installed into")
	}
}

// Codex has no hook or statusline surface: its whole integration is the MCP
// server, delegated to its own cli.
func TestCodexHostIsMCPOnly(t *testing.T) {
	h := newHostFixture(t, opSettings)
	args := []string{"codex", "-home", h.home, "-bin", h.bin, "-json"}
	rep, err := capture(t, func() error { return runInstall(args) })
	if err != nil {
		t.Fatalf("install codex: %v (%+v)", err, rep)
	}
	if len(rep.Steps) != 1 || rep.Steps[0].Kind != hostcfg.StepHostCLI {
		t.Fatalf("codex should have exactly one delegated step: %+v", rep.Steps)
	}
	if h.read(t) != opSettings {
		t.Fatal("installing the codex host must not touch Claude Code's settings")
	}
	cfg := filepath.Join(h.home, ".codex", "config.toml")
	p, _, err := probeCodexMCP(cfg)
	if err != nil || p != hostcfg.PresentFile {
		t.Fatalf("codex server not registered: presence=%v err=%v", p, err)
	}
	if _, err := capture(t, func() error { return runUninstall(args) }); err != nil {
		t.Fatalf("uninstall codex: %v", err)
	}
	p, _, err = probeCodexMCP(cfg)
	if err != nil || p != hostcfg.AbsentFile {
		t.Fatalf("codex server not deregistered: presence=%v err=%v", p, err)
	}
}

func TestUnknownHostIsRejected(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error {
		return runInstall([]string{"emacs", "-home", h.home, "-bin", h.bin, "-json"})
	}); err == nil {
		t.Fatal("an unknown host must be rejected")
	}
	if _, err := capture(t, func() error {
		return runUninstall([]string{"emacs", "-home", h.home, "-bin", h.bin, "-json"})
	}); err == nil {
		t.Fatal("an unknown host must be rejected by uninstall too")
	}
}

// -home must isolate the WHOLE install, not just the host config. When only
// settings.json honoured it, a scratch run wrote its manifest, backups, tee and
// delegate into the operator's real ~/.meta-router — planting an ownership
// record for steps that were not in the real config, which then made the real
// install refuse permanently.
func TestHomeIsolatesEverything(t *testing.T) {
	h := newHostFixture(t, opSettings)
	elsewhere := os.Getenv("MR_ORCH_STATE") // where a half-isolated install would write
	if _, err := capture(t, func() error {
		// No -bin either: its default must also derive from -home.
		return runInstall([]string{"claude", "-home", h.home, "-json"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		hostcfg.ManifestPath(h.state, "claude"),
		filepath.Join(h.state, "statusline-delegate.json"),
		filepath.Join(hostBinDir(h.home), teeFileName),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should live under -home: %v", p, err)
		}
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("install wrote outside -home, into %s: %v", elsewhere, entries)
	}
}

// The four-step flow that used to poison every later uninstall: install, let
// the operator change the config, install again (writing settings.json a second
// time), uninstall. The second backup must NOT overwrite the first.
func TestSecondInstallKeepsTheOriginalBackup(t *testing.T) {
	noStatusline := `{
  "model": "opus",
  "_note": "hand-written"
}
`
	h := newHostFixture(t, noStatusline)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatalf("install #1: %v", err)
	}
	// The operator adds a statusline, so a second install has real work to do.
	d, _, err := hostcfg.LoadDoc(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MemberSet([]string{"statusLine"},
		json.RawMessage(`{"type":"command","command":"node theirs.js"}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	withStatusline := h.read(t)

	rep2, err := capture(t, func() error { return runInstall(h.args()) })
	if err != nil {
		t.Fatalf("install #2: %v (%+v)", err, rep2)
	}
	if statusOf(rep2, "statusLine") != stWire {
		t.Fatalf("install #2 should have had work to do: %+v", rep2.Steps)
	}
	rep3, err := capture(t, func() error { return runUninstall(h.args()) })
	if err != nil {
		t.Fatalf("uninstall after two installs must not hard-fail: %v (%+v)", err, rep3)
	}
	// Byte-identical means back to what install #1 found — not to the
	// half-installed state install #2 started from.
	if got := h.read(t); got != noStatusline {
		t.Fatalf("uninstall restored the wrong baseline.\n--- want (pre-install-#1) ---\n%s\n--- got ---\n%s\n(the operator's own later edit was: %s)",
			noStatusline, got, withStatusline)
	}
}

// An entry of OURS that no longer decodes is not an absent entry. Reading it as
// "already gone" made uninstall report ok, delete the manifest, and leave the
// hook wired with nothing on disk claiming ownership of it.
func TestUnparseableOwnEntryRefusesInsteadOfReportingGone(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	raw := h.read(t)
	// Retype our own hook's numeric timeout as a string — a plausible operator
	// or host-schema edit that makes the element fail to decode.
	edited := strings.Replace(raw, `"timeout": 5`, `"timeout": "5s"`, 1)
	if edited == raw {
		t.Fatalf("fixture drift: no timeout field to retype in\n%s", raw)
	}
	if err := os.WriteFile(h.settings, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err == nil {
		t.Fatal("an unparseable entry must refuse, not report 'already gone'")
	}
	if statusOf(rep, "UserPromptSubmit") != stDrift {
		t.Fatalf("expected drift on the unparseable entry: %+v", rep.Steps)
	}
	if _, err := os.Stat(h.manifestPath()); err != nil {
		t.Fatalf("a refused uninstall must keep the ownership record: %v", err)
	}
	if got := h.read(t); got != edited {
		t.Fatal("a refused uninstall must write NOTHING")
	}
}

// A statusLine that exists but whose command we cannot read is not something to
// wrap: wiring the tee over it recorded an empty delegate, and the tee then
// rendered nothing — a blank statusline from a run that reported success.
func TestTeeSkippedWhenTheStatuslineCommandIsUnreadable(t *testing.T) {
	for name, sl := range map[string]string{
		"object-without-command": `"statusLine": {"type": "static", "text": "hi"}`,
		"bare-string-form":       `"statusLine": "node theirs-statusline.js"`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHostFixture(t, "{\n  \"model\": \"opus\",\n  "+sl+"\n}\n")
			rep, err := capture(t, func() error { return runInstall(h.args()) })
			if err != nil {
				t.Fatalf("install: %v (%+v)", err, rep)
			}
			if got := statusOf(rep, "statusLine"); got != stSkip {
				t.Fatalf("expected the tee to be skipped, got %q: %+v", got, rep.Steps)
			}
			if _, err := os.Stat(filepath.Join(h.state, "statusline-delegate.json")); !os.IsNotExist(err) {
				t.Fatalf("no delegate should have been written: %v", err)
			}
			// The member must survive byte-for-byte: untouched, not rewritten.
			if got := h.read(t); !strings.Contains(got, sl) {
				t.Fatalf("the operator's statusLine was replaced or rewritten:\n%s", got)
			}
		})
	}
}

// The registry undo must be probed, not assumed. `claude mcp remove` exits
// non-zero for a server that is already gone, which aborted the whole uninstall
// with settings.json still fully wired — permanently, on every retry.
func TestUninstallToleratesAnAlreadyDeregisteredServer(t *testing.T) {
	h := newHostFixture(t, opSettings)
	if _, err := capture(t, func() error { return runInstall(h.args()) }); err != nil {
		t.Fatal(err)
	}
	// The operator removes the MCP entry by hand.
	if code := stubClaudeMCP(filepath.Join(h.home, ".claude.json"), "remove"); code != 0 {
		t.Fatalf("stub remove failed: %d", code)
	}
	rep, err := capture(t, func() error { return runUninstall(h.args()) })
	if err != nil {
		t.Fatalf("uninstall must not abort over a server that is already gone: %v (%+v)", err, rep)
	}
	if got := statusOf(rep, "MCP server"); got != stNotWired {
		t.Fatalf("expected the registry step to report not-wired, got %q", got)
	}
	if got := h.read(t); got != opSettings {
		t.Fatalf("the config should still have been fully restored:\n%s", got)
	}
}

func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func jsonEsc(s string) string {
	q := mustQuote(s)
	return q[1 : len(q)-1]
}
