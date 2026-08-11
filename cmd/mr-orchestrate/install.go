package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/meta-router/internal/orch/hostcfg"
)

// W7a — host installers.
//
// The DX gap this closes: wiring meta-router into a host has been a hand
// exercise (the statusline-tee keystroke sat operator-pending for weeks), which
// does not scale to a fleet and leaves every machine wired slightly differently.
//
// Two rules shape the whole design.
//
// (1) OWNERSHIP IS RECORDED, NEVER INFERRED. The manifest under
// <state>/install/<host>.json is the only authority for what this command may
// later remove. An entry that merely looks like ours but is absent from the
// manifest belongs to someone else — a hand edit, another tool, an older
// install — and the installer refuses rather than adopting it. Guessing is how
// an installer deletes an operator's own hook and calls it cleanup.
//
// (2) WE ONLY EDIT FILES THE HOST DOES NOT WRITE CONCURRENTLY. Claude Code's
// hooks and statusLine live in ~/.claude/settings.json, which the host reads but
// does not continuously rewrite, so mr-orchestrate edits it directly. Both MCP
// registries — ~/.claude.json and ~/.codex/config.toml — are LIVE STATE the host
// writes while it runs (project entries, skill usage, trust levels), so a
// read-modify-write from a second process would silently drop whatever the host
// wrote in between. Those go through the host's own `mcp add` / `mcp remove`.
// It also means no hand-rolled TOML editing in a repo with no TOML parser.

//go:embed assets/mr-statusline-tee.js
var statuslineTeeJS []byte

const (
	mcpServerName = "meta-router"
	teeFileName   = "mr-statusline-tee.js"
)

// Step statuses, shared by install and uninstall reporting.
const (
	stWire      = "wire"             // not present; install will add it
	stManaged   = "managed"          // manifest owns it and it is intact
	stModified  = "managed-modified" // manifest owns it; still ours, but edited since
	stConflict  = "conflict"         // present and NOT ours — refuse
	stDrift     = "drift"            // manifest claims it, but it is gone or ambiguous — refuse
	stSkip      = "skip"             // precondition not met
	stRemove    = "remove"           // uninstall will remove it
	stNotWired  = "not-wired"        // nothing recorded and nothing present
	stUndelegab = "undelegated"      // a host-cli step whose CLI is unavailable
)

// refuses is the ONE definition of "this status must abort the run".
//
// It was previously spelled out at nine call sites in two files, and the two
// spellings had already diverged — the central predicate omitted stUndelegab
// while the loop that produced it set the flag inline. One function means the
// next status added cannot be forgotten in half the places.
func refuses(status string) bool {
	switch status {
	case stConflict, stDrift, stUndelegab:
		return true
	}
	return false
}

// editStep is one JSON-config edit we perform ourselves.
type editStep struct {
	kind  string // hostcfg.StepArrayAppend | hostcfg.StepMemberSet
	desc  string
	file  string
	path  []string
	value json.RawMessage
	// ident is the substring of a "command" field that identifies OUR entry
	// among its neighbours. Identity is the binary/script we invoke, never the
	// whole entry: an operator who tunes a flag on our hook has not taken it
	// over, and treating that as drift would wedge every later uninstall.
	ident string
	// requiresExisting marks a step that only makes sense when the target is
	// already present — the statusline tee has nothing to tee to otherwise, and
	// installing it over an absent statusLine would replace the host's built-in
	// default with a blank line.
	requiresExisting bool
	// isTee marks the step that wires the statusline tee, so apply knows to
	// write the tee's own two files. Naming the step beats sniffing its ident,
	// which broke the moment the ident became a full path.
	isTee bool
}

// cliStep is one change delegated to the host's own CLI.
type cliStep struct {
	desc string
	bin  string
	run  []string
	undo []string
	// probe answers "is this already registered?" by READING the host's config,
	// never by running the CLI: a probe that spawns the host is slow, and its
	// failure would be indistinguishable from "not registered".
	probe func() (hostcfg.Presence, string, error)
}

type hostPlan struct {
	host     string
	edits    []editStep
	clis     []cliStep
	teeAsset string // absolute path the embedded tee is written to ("" = none)
	delegate string // where the tee reads the command it wraps
}

type stepReport struct {
	Desc   string `json:"desc"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type installReport struct {
	Host     string       `json:"host"`
	Action   string       `json:"action"`
	DryRun   bool         `json:"dry_run"`
	BinDir   string       `json:"bin_dir"`
	Manifest string       `json:"manifest"`
	Steps    []stepReport `json:"steps"`
	Files    []string     `json:"files_written,omitempty"`
	Restore  string       `json:"restore,omitempty"`
	Result   string       `json:"result"`
	Note     string       `json:"note"`
}

// hostBinDir and hostStateDir derive from the HOME BEING WIRED, never from the
// invoking user's home or from MR_ORCH_STATE.
//
// That is not a detail. When only the host config honoured -home, a scratch
// install wrote its manifest, its pre-install backups, the tee script and the
// statusline delegate into the OPERATOR's real ~/.meta-router — planting an
// ownership record for steps that were never in the real settings.json, which
// then made the real install refuse permanently. The install's whole state
// belongs to the home it is wiring; anything else is not isolation, it is a
// half-isolated write into somebody's live config.
func hostBinDir(home string) string   { return filepath.Join(home, ".meta-router", "bin") }
func hostStateDir(home string) string { return filepath.Join(home, ".meta-router", "orchestrate") }

// exeName appends the platform's executable suffix. The fleet is Windows today
// but the binaries are cross-built, and an installer that hard-codes .exe would
// write an unreachable path on a Linux node.
func exeName(base string) string {
	if os.PathListSeparator == ';' { // Windows
		return base + ".exe"
	}
	return base
}

// planFor builds the ordered, host-specific set of changes.
func planFor(host, home, binDir, stateDir string) (*hostPlan, error) {
	orchBin := filepath.Join(binDir, exeName("mr-orchestrate"))
	switch host {
	case "claude":
		settings := filepath.Join(home, ".claude", "settings.json")
		hookBin := filepath.Join(binDir, exeName("mr-hook"))
		indexBin := filepath.Join(binDir, exeName("mr-index"))
		tee := filepath.Join(binDir, teeFileName)
		p := &hostPlan{host: host, teeAsset: tee,
			delegate: filepath.Join(stateDir, "statusline-delegate.json")}
		p.edits = append(p.edits,
			editStep{
				kind: hostcfg.StepArrayAppend,
				desc: "UserPromptSubmit hook → mr-hook (per-prompt skill surfacing)",
				file: settings, path: []string{"hooks", "UserPromptSubmit"},
				value: mustJSON(hookGroup{Hooks: []hookEntry{{
					Type: "command", Command: hookBin, Timeout: 5,
					StatusMessage: "meta-router: surfacing skills",
				}}}),
				ident: hookBin,
			},
			editStep{
				kind: hostcfg.StepArrayAppend,
				desc: "SessionStart hook → mr-index refresh (hash-diff re-embed)",
				file: settings, path: []string{"hooks", "SessionStart"},
				value: mustJSON(hookGroup{Hooks: []hookEntry{{
					Type: "command", Command: indexBin, Args: []string{"refresh"},
					Timeout: 60, Async: true,
				}}}),
				ident: indexBin,
			},
			editStep{
				kind: hostcfg.StepMemberSet,
				desc: "statusLine → mr-statusline-tee (quota signal), delegating to the existing statusline",
				file: settings, path: []string{"statusLine"},
				value: mustJSON(map[string]string{
					"type":    "command",
					"command": teeCommand(tee, stateDir),
				}),
				// The FULL path, not the bare filename. A filename matches a tee
				// in any bin dir, so moving -bin left the statusLine reported as
				// `managed` while it still pointed at the old, now-deleted script.
				ident:            filepath.ToSlash(tee),
				requiresExisting: true,
				isTee:            true,
			},
		)
		p.clis = append(p.clis, cliStep{
			desc: "MCP server 'meta-router' registered with Claude Code (user scope)",
			bin:  "claude",
			run:  []string{"mcp", "add", "--scope", "user", mcpServerName, "--", orchBin, "mcp"},
			undo: []string{"mcp", "remove", "--scope", "user", mcpServerName},
			probe: func() (hostcfg.Presence, string, error) {
				return probeClaudeMCP(filepath.Join(home, ".claude.json"))
			},
		})
		return p, nil
	case "codex":
		// Codex has no hook or statusline surface: its whole integration is the
		// MCP server. Saying so beats inventing a step to look symmetric.
		p := &hostPlan{host: host}
		p.clis = append(p.clis, cliStep{
			desc: "MCP server 'meta-router' registered with Codex CLI",
			bin:  "codex",
			run:  []string{"mcp", "add", mcpServerName, "--", orchBin, "mcp"},
			undo: []string{"mcp", "remove", mcpServerName},
			probe: func() (hostcfg.Presence, string, error) {
				return probeCodexMCP(filepath.Join(home, ".codex", "config.toml"))
			},
		})
		return p, nil
	default:
		return nil, fmt.Errorf("unknown host %q (supported: claude, codex)", host)
	}
}

type hookEntry struct {
	Type          string   `json:"type"`
	Command       string   `json:"command"`
	Args          []string `json:"args,omitempty"`
	Timeout       int      `json:"timeout,omitempty"`
	StatusMessage string   `json:"statusMessage,omitempty"`
	Async         bool     `json:"async,omitempty"`
}

type hookGroup struct {
	Hooks []hookEntry `json:"hooks"`
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil { // only reachable from a non-serialisable literal above
		panic(err)
	}
	return b
}

// teeCommand renders the statusLine command line that runs the tee.
//
// node is resolved at RUN time by the host's shell, not pinned here: pinning an
// absolute interpreter path is what killed a capture task for eight days when a
// versioned Store path vanished on update.
//
// The state dir is passed as an ARGUMENT rather than left to MR_ORCH_STATE.
// The tee runs inside the host's process, which need not carry that variable —
// so an install that resolved a state dir from the environment wired a tee that
// would look somewhere else entirely at render time, find no delegate, and
// print nothing.
func teeCommand(teePath, stateDir string) string {
	return `node "` + filepath.ToSlash(teePath) + `" "` + filepath.ToSlash(stateDir) + `"`
}

// commandsIn extracts every "command" string inside a hook-group element, and
// reports separately whether the element could be READ at all.
//
// The second return value is the point. An element that fails to decode yields
// no commands, and code that reads "no commands" as "not ours" would answer
// "already gone" for an entry it simply could not parse — then delete the
// manifest, leaving our hook wired with nothing on disk claiming it. That is
// the absent-versus-undetermined trap, in the one place it costs the most.
// One plausible operator edit reaches it: retyping our `"timeout": 5` as
// `"timeout": "5s"`.
func commandsIn(raw json.RawMessage) (cmds []string, parsed bool) {
	var g hookGroup
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(g.Hooks))
	for _, h := range g.Hooks {
		out = append(out, h.Command)
	}
	return out, true
}

// memberCommand extracts the "command" field of a single-object member such as
// statusLine.
func memberCommand(raw json.RawMessage) string {
	var m struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return m.Command
}

// findOurs locates the element in an array that invokes ident.
//
// It answers with three distinct outcomes rather than two, because "we did not
// find it" and "we could not tell" call for opposite actions:
//   - idx >= 0        — found (ambiguous when a second element also invokes ident:
//     that is a state no install of ours produces, so picking one would be a coin flip)
//   - idx == -1, undetermined == false — provably not there
//   - undetermined == true             — at least one element could not be parsed,
//     so absence is unproven and no destructive action may follow
func findOurs(d *hostcfg.Doc, path []string, ident string) (idx int, ambiguous, undetermined bool, err error) {
	n, present, err := d.ArrayLen(path)
	if err != nil || !present {
		return -1, false, false, err
	}
	idx = -1
	for i := 0; i < n; i++ {
		raw, e := d.ArrayAt(path, i)
		if e != nil {
			return -1, false, false, e
		}
		cmds, ok := commandsIn(raw)
		if !ok {
			undetermined = true
			continue
		}
		hit := false
		for _, c := range cmds {
			if c == ident {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if idx >= 0 {
			return idx, true, undetermined, nil
		}
		idx = i
	}
	// An unparseable neighbour only matters when we came up empty: once ours is
	// located, what the neighbours are is not our business.
	if idx >= 0 {
		undetermined = false
	}
	return idx, false, undetermined, nil
}

// probeClaudeMCP reads Claude Code's user-scope registry WITHOUT writing to it.
func probeClaudeMCP(path string) (hostcfg.Presence, string, error) {
	b, p, err := hostcfg.ReadFile(path)
	if err != nil || p != hostcfg.PresentFile {
		return p, "", err
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return hostcfg.UnknownFile, "", fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := doc.MCPServers[mcpServerName]; ok {
		return hostcfg.PresentFile, path, nil
	}
	return hostcfg.AbsentFile, path, nil
}

// probeCodexMCP looks for the server's TOML table header. A header scan, not a
// parse: this repo ships no TOML parser, and presence is all the probe needs.
func probeCodexMCP(path string) (hostcfg.Presence, string, error) {
	b, p, err := hostcfg.ReadFile(path)
	if err != nil || p != hostcfg.PresentFile {
		return p, "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "[mcp_servers."+mcpServerName+"]" ||
			t == `[mcp_servers."`+mcpServerName+`"]` ||
			t == "[mcp_servers.'"+mcpServerName+"']" {
			return hostcfg.PresentFile, path, nil
		}
	}
	return hostcfg.AbsentFile, path, nil
}

// stepFromManifest returns the recorded step for a path, if any.
func stepFromManifest(m *hostcfg.Manifest, kind string, path []string) *hostcfg.Step {
	if m == nil {
		return nil
	}
	for i := range m.Steps {
		s := &m.Steps[i]
		if s.Kind != kind || len(s.Path) != len(path) {
			continue
		}
		same := true
		for j := range path {
			if s.Path[j] != path[j] {
				same = false
				break
			}
		}
		if same {
			return s
		}
	}
	return nil
}

func cliStepFromManifest(m *hostcfg.Manifest, desc string) *hostcfg.Step {
	if m == nil {
		return nil
	}
	for i := range m.Steps {
		if m.Steps[i].Kind == hostcfg.StepHostCLI && m.Steps[i].Desc == desc {
			return &m.Steps[i]
		}
	}
	return nil
}

func usageInstall() string {
	return `usage: mr-orchestrate install|uninstall <claude|codex> [flags]
  -home <dir>    host home to wire (default: this user's home). The install
                 manifest, its backups and the tee all live under <home>, so a
                 scratch home is fully isolated from the operator's own.
  -bin <dir>     deployed mr-* binaries (default: <home>/.meta-router/bin)
  -dry-run       report exactly what would change and write NOTHING
  -json          machine-readable report`
}

type installFlags struct {
	host     string
	home     string
	binDir   string
	stateDir string
	dryRun   bool
	asJSON   bool
}

func parseInstallArgs(name string, args []string) (*installFlags, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return nil, fmt.Errorf("%s needs a host\n%s", name, usageInstall())
	}
	f := &installFlags{host: args[0]}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&f.home, "home", "", "host home directory")
	fs.StringVar(&f.binDir, "bin", "", "deployed binaries directory")
	fs.BoolVar(&f.dryRun, "dry-run", false, "report only; write nothing")
	fs.BoolVar(&f.asJSON, "json", false, "machine-readable report")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, err
	}
	if f.home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		f.home = h
	}
	if f.binDir == "" {
		f.binDir = hostBinDir(f.home)
	}
	f.stateDir = hostStateDir(f.home)
	return f, nil
}

func emitReport(rep *installReport, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			// Silently dropping this produced empty stdout AND exit 0 — a caller
			// parsing the report would read "nothing happened" from a run that
			// rewrote a config.
			fmt.Fprintln(os.Stderr, "error: could not write the report:", err)
			os.Exit(1)
		}
		return
	}
	// The header must not say "applied" for a run that refused or failed — the
	// first line is the one an operator actually reads.
	state := "applied"
	switch {
	case rep.DryRun:
		state = "dry run"
	case rep.Result == "refused":
		state = "REFUSED — nothing written"
	case rep.Result == "failed":
		state = "FAILED"
	}
	fmt.Printf("%s %s (%s)\n", rep.Action, rep.Host, state)
	fmt.Println("  bin:      ", rep.BinDir)
	fmt.Println("  manifest: ", rep.Manifest)
	for _, s := range rep.Steps {
		line := fmt.Sprintf("  [%-16s] %s", s.Status, s.Desc)
		if s.Detail != "" {
			line += "\n" + strings.Repeat(" ", 21) + s.Detail
		}
		fmt.Println(line)
	}
	// Files was previously printed only under -json, which hid the one line that
	// matters most: a file LEFT IN PLACE because it changed since install. The
	// operator was told "ok" and never learned something had been orphaned.
	for _, f := range rep.Files {
		fmt.Println("  file:     ", f)
	}
	if rep.Restore != "" {
		fmt.Println("  restore:  ", rep.Restore)
	}
	fmt.Println("  result:   ", rep.Result)
	fmt.Println("  ", rep.Note)
}

// movedBinDetail explains a drift whose real cause is almost always a moved
// -bin, and names the remedy that WORKS. The generic advice — delete the
// manifest and re-run — is actively harmful here: it drops the only record of
// the old entry, and the re-run then appends a SECOND hook beside a stale one
// pointing at a binary that no longer exists.
func movedBinDetail(rec *hostcfg.Step, e *editStep, where string) string {
	if rec.Ident != "" && rec.Ident != e.ident {
		return fmt.Sprintf("the recorded entry invokes %s but this install targets %s — run `mr-orchestrate uninstall` FIRST (it reverses the recorded paths), then install against the new -bin",
			rec.Ident, e.ident)
	}
	return fmt.Sprintf("the manifest records this entry but %s no longer invokes %s", where, e.ident)
}
