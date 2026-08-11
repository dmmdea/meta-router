package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/childenv"
	"github.com/dmmdea/meta-router/internal/orch/hostcfg"
)

// decision is one assessed step plus everything apply/revert needs.
type decision struct {
	edit        *editStep
	cli         *cliStep
	desc        string // copied at construction: reporting must not depend on which pointer is set
	kind        string
	status      string
	detail      string
	prior       json.RawMessage
	priorAbsent bool
	// carried is the manifest record this step already had, preserved verbatim
	// when the step is a no-op so a re-install never loses the ORIGINAL prior
	// value (the one an uninstall has to restore).
	carried *hostcfg.Step
}

// refusalNote names a remedy that actually applies to the refusal at hand.
//
// A single generic note was wrong in the two cases operators actually hit: it
// told a FIRST-time installer to delete a manifest that does not exist, and it
// told an operator whose -bin had moved to delete the very record that made a
// clean uninstall possible — after which the re-run appended a second hook next
// to the stale one.
func refusalNote(action, host, manifest string, haveManifest, undelegated bool) string {
	note := "refused: nothing was written. "
	switch {
	case undelegated:
		note += "The host's own CLI is unavailable, and its MCP registry is live state only that CLI may write. " +
			"Put it on PATH and re-run; nothing else here is wired without it."
	case action == "uninstall":
		// Telling an uninstall to "run uninstall first" is the kind of advice
		// that reads as a bug. The only real remedies here are the operator's.
		note += "The entries marked above are no longer the ones this installer wrote, and removing them anyway " +
			"would destroy an operator setting. Either restore them to what the manifest records, remove them by hand, " +
			"or delete " + manifest + " to drop the ownership claim entirely — after which nothing here is managed."
	case haveManifest:
		note += "The entries above no longer match what this installer recorded. " +
			"Run `mr-orchestrate uninstall " + host + "` first — it reverses the RECORDED paths, which is what you want after moving -bin — " +
			"or resolve them by hand. Deleting " + manifest + " drops the ownership claim, but then those entries are yours to remove."
	default:
		note += "Something else already occupies these entries and no manifest records them, so this installer will not adopt or overwrite them. " +
			"Remove them by hand and re-run."
	}
	return note
}

// runInstall wires meta-router into a host. It is ALL-OR-NOTHING: every step is
// assessed read-only first, a single conflict or drift refuses the whole run,
// and a failure part-way restores what was already written.
func runInstall(args []string) error {
	f, err := parseInstallArgs("install", args)
	if err != nil {
		return err
	}
	stateDir := f.stateDir
	plan, err := planFor(f.host, f.home, f.binDir, stateDir)
	if err != nil {
		return err
	}
	manPath := hostcfg.ManifestPath(stateDir, f.host)
	man, _, err := hostcfg.LoadManifest(manPath)
	if err != nil {
		return err // an unreadable manifest must never read as "never installed"
	}

	rep := &installReport{Host: f.host, Action: "install", DryRun: f.dryRun,
		BinDir: f.binDir, Manifest: manPath}

	docs := map[string]*hostcfg.Doc{}
	for _, e := range plan.edits {
		if _, ok := docs[e.file]; ok {
			continue
		}
		d, _, err := hostcfg.LoadDoc(e.file)
		if err != nil {
			return fmt.Errorf("read host config: %w", err)
		}
		docs[e.file] = d
	}

	decisions, refuse, err := assessInstall(plan, docs, man)
	if err != nil {
		return err
	}
	pending, skipped, undelegated := 0, 0, false
	for _, d := range decisions {
		rep.Steps = append(rep.Steps, stepReport{Desc: d.desc, Kind: d.kind,
			Status: d.status, Detail: d.detail})
		switch d.status {
		case stWire:
			pending++
		case stSkip, stModified:
			skipped++
		case stUndelegab:
			undelegated = true
		}
	}
	if refuse {
		rep.Result = "refused"
		rep.Note = refusalNote("install", f.host, manPath, man != nil, undelegated)
		emitReport(rep, f.asJSON)
		return fmt.Errorf("install refused: host config holds entries this installer does not own")
	}

	// "already wired" must not be said while a step was skipped: the tee IS the
	// quota signal, and reporting a complete install without it sent the
	// operator away believing they had something they did not.
	incomplete := ""
	if skipped > 0 {
		incomplete = fmt.Sprintf(" %d step(s) were skipped or edited since install and are NOT wired — see above.", skipped)
	}
	if pending == 0 {
		rep.Result = "ok"
		rep.Note = "nothing left to wire." + incomplete
		emitReport(rep, f.asJSON)
		return nil
	}
	if f.dryRun {
		rep.Result = "ok"
		rep.Note = fmt.Sprintf("dry run: %d step(s) would be wired. Nothing was written.%s", pending, incomplete)
		emitReport(rep, f.asJSON)
		return nil
	}
	if err := applyInstall(f, plan, docs, man, decisions, rep, manPath, stateDir); err != nil {
		// The report is the only durable record of what was touched, so it is
		// emitted on the failure path too — a -json caller previously got empty
		// stdout and a stderr string it could not parse.
		rep.Result = "failed"
		rep.Note = "install failed: " + err.Error()
		emitReport(rep, f.asJSON)
		return err
	}
	rep.Result = "ok"
	rep.Note = fmt.Sprintf("wired %d step(s). `mr-orchestrate uninstall %s` reverses exactly these.%s", pending, f.host, incomplete)
	emitReport(rep, f.asJSON)
	return nil
}

// assessInstall decides every step WITHOUT writing anything.
func assessInstall(plan *hostPlan, docs map[string]*hostcfg.Doc, man *hostcfg.Manifest) ([]decision, bool, error) {
	var out []decision
	refuse := false
	for i := range plan.edits {
		e := &plan.edits[i]
		d := docs[e.file]
		rec := stepFromManifest(man, e.kind, e.path)
		dec := decision{edit: e, desc: e.desc, kind: e.kind, carried: rec}
		where := strings.Join(e.path, ".")
		switch e.kind {
		case hostcfg.StepArrayAppend:
			idx, amb, undetermined, err := findOurs(d, e.path, e.ident)
			if err != nil {
				return nil, false, err
			}
			switch {
			case amb:
				dec.status, dec.detail = stConflict,
					fmt.Sprintf("two entries in %s invoke %s — remove one by hand", where, e.ident)
			case idx >= 0 && rec == nil:
				dec.status, dec.detail = stConflict,
					fmt.Sprintf("%s already invokes %s, but no manifest records it — this installer does not adopt entries it did not write", where, e.ident)
			case idx >= 0:
				cur, err := d.ArrayAt(e.path, idx)
				if err != nil {
					return nil, false, err
				}
				same, err := hostcfg.SameValue(cur, rec.Installed)
				if err != nil || !same {
					dec.status, dec.detail = stModified, "recorded as ours and still invokes our binary, but has been edited since install"
				} else {
					dec.status = stManaged
				}
			case undetermined:
				dec.status, dec.detail = stDrift,
					fmt.Sprintf("an entry in %s could not be parsed, so whether ours is already there is unknown — not the same thing as absent", where)
			case rec != nil:
				dec.status, dec.detail = stDrift, movedBinDetail(rec, e, where)
			default:
				dec.status = stWire
			}
		case hostcfg.StepMemberSet:
			cur, present, err := d.MemberGet(e.path)
			if err != nil {
				return nil, false, err
			}
			priorCmd := ""
			if present {
				priorCmd = memberCommand(cur)
			}
			ours := present && strings.Contains(priorCmd, e.ident)
			switch {
			case ours && rec == nil:
				dec.status, dec.detail = stConflict,
					fmt.Sprintf("%s already runs %s, but no manifest records it — remove it by hand to let this installer own it", where, e.ident)
			case ours:
				same, err := hostcfg.SameValue(cur, rec.Installed)
				if err != nil || !same {
					dec.status, dec.detail = stModified, "recorded as ours and still runs our script, but has been edited since install"
				} else {
					dec.status = stManaged
				}
			case rec != nil:
				dec.status, dec.detail = stDrift,
					fmt.Sprintf("the manifest records %s as ours, but it now holds something else", where)
			// The tee RELAYS the statusline it replaces, so it needs a command to
			// relay TO. Wiring it over an absent statusLine replaces the host's
			// built-in default with a blank line; wiring it over a statusLine whose
			// command we cannot read (a `static` type, or the bare-string form)
			// records an empty delegate, and the tee then renders nothing at all.
			// Both are silent downgrades of something the operator can see, so
			// both skip.
			case e.requiresExisting && priorCmd == "":
				dec.status = stSkip
				if present {
					dec.detail = "the existing statusLine has no `command` for the tee to delegate to, so wiring it would blank the status line"
				} else {
					dec.detail = "no existing statusLine to wrap — installing the tee over nothing would blank the host's built-in default"
				}
			default:
				dec.status = stWire
				dec.prior, dec.priorAbsent = cur, !present
			}
		}
		if refuses(dec.status) {
			refuse = true
		}
		out = append(out, dec)
	}

	for i := range plan.clis {
		c := &plan.clis[i]
		rec := cliStepFromManifest(man, c.desc)
		dec := decision{cli: c, desc: c.desc, kind: hostcfg.StepHostCLI, carried: rec}
		p, where, err := c.probe()
		if err != nil {
			return nil, false, fmt.Errorf("probe %s: %w", c.desc, err)
		}
		switch {
		case p == hostcfg.UnknownFile:
			// Never "absent": an unreadable registry is a reason to stop, not a
			// licence to register a second copy.
			dec.status, dec.detail, refuse = stDrift, "could not read the host's MCP registry", true
		case p == hostcfg.PresentFile && rec == nil:
			dec.status, dec.detail, refuse = stConflict,
				fmt.Sprintf("%q is already registered in %s, but no manifest records it", mcpServerName, where), true
		case p == hostcfg.PresentFile:
			dec.status = stManaged
		case rec != nil:
			dec.status, dec.detail, refuse = stDrift,
				fmt.Sprintf("the manifest records %q as registered, but it is not in the host's registry", mcpServerName), true
		default:
			if _, err := exec.LookPath(c.bin); err != nil {
				dec.status, dec.detail, refuse = stUndelegab,
					fmt.Sprintf("%s is not on PATH — this registry belongs to the host and is only ever written by its own CLI", c.bin), true
				break
			}
			dec.status = stWire
		}
		out = append(out, dec)
	}
	return out, refuse, nil
}

// applyInstall performs the writes, rolling back everything on any failure.
func applyInstall(f *installFlags, plan *hostPlan, docs map[string]*hostcfg.Doc,
	man *hostcfg.Manifest, decisions []decision, rep *installReport, manPath, stateDir string) error {

	newMan := &hostcfg.Manifest{Schema: hostcfg.ManifestSchema, Host: f.host,
		MRVersion: version, InstalledAt: time.Now().UTC().Format(time.RFC3339),
		BinDir: f.binDir}

	// Every file we are about to change, backed up before the first write so a
	// rollback (and a later uninstall) has the exact original bytes.
	var files []touchedFile
	var undoCLI [][]string
	var undoBins []string
	var undoEnvs [][]string
	// rollback undoes everything this run wrote, and REPORTS when it could not.
	// It returns the leftovers rather than only warning: a rollback that fails
	// silently leaves wiring no later command can see (the manifest is never
	// written, so `uninstall` truthfully answers "nothing recorded to remove")
	// and the only trace was a stderr line that dies with the scrollback.
	rollback := func() []string {
		var stranded []string
		for i := len(undoCLI) - 1; i >= 0; i-- {
			if err := runHostCLI(undoBins[i], undoCLI[i], undoEnvs[i]); err != nil {
				stranded = append(stranded, fmt.Sprintf("`%s %s` did not reverse: %v",
					undoBins[i], strings.Join(undoCLI[i], " "), err))
			}
		}
		// REVERSE order, so a config that references a file we created is put
		// back before that file is deleted. Forward order deleted the tee script
		// first, and a settings.json restore that then failed left the statusline
		// running `node <deleted file>` on every render.
		for i := len(files) - 1; i >= 0; i-- {
			t := files[i]
			var err error
			remedy := "restore it from " + t.backup
			if t.existed {
				err = hostcfg.WriteAtomic(t.path, t.before, 0o644)
			} else {
				remedy = "delete it" // we created it; there is no pre-install copy
				if err = os.Remove(t.path); os.IsNotExist(err) {
					err = nil
				}
			}
			if err != nil {
				stranded = append(stranded, fmt.Sprintf("%s could not be rolled back (%v) — %s", t.path, err, remedy))
			}
		}
		for _, s := range stranded {
			fmt.Fprintln(os.Stderr, "WARN: rollback incomplete:", s)
		}
		return stranded
	}
	failWith := func(err error) error {
		if stranded := rollback(); len(stranded) > 0 {
			rep.Files = append(rep.Files, stranded...)
			return fmt.Errorf("%w — AND ROLLBACK WAS INCOMPLETE, manual repair required: %s",
				err, strings.Join(stranded, "; "))
		}
		return err
	}

	priorRec := func(path string) *hostcfg.FileRec {
		if man == nil {
			return nil
		}
		for i := range man.Files {
			if man.Files[i].Path == path {
				return &man.Files[i]
			}
		}
		return nil
	}

	backup := func(path string) (touchedFile, error) {
		before, p, err := hostcfg.ReadFile(path)
		if err != nil {
			return touchedFile{}, err
		}
		t := touchedFile{path: path, before: before, existed: p == hostcfg.PresentFile,
			backup: hostcfg.BackupPath(stateDir, f.host, filepath.Base(path))}
		if !t.existed {
			return t, nil
		}
		// t.before is this RUN's starting bytes, which is what a rollback owes
		// the operator. The persisted .pre-install copy is a different thing: the
		// bytes before the FIRST install, which is what an uninstall owes them.
		//
		// Conflating the two poisoned every later uninstall. A second install
		// that wired one more step overwrote the .pre-install copy with the
		// ALREADY-INSTALLED bytes, while carryOriginal kept the first install's
		// hash — so the restore's integrity check compared a hash and a file that
		// could never match again, and uninstall hard-failed permanently with the
		// hooks still wired.
		if pr := priorRec(path); pr != nil && pr.Backup != "" {
			if _, bp, _ := hostcfg.ReadFile(pr.Backup); bp == hostcfg.PresentFile {
				return t, nil
			}
		}
		if err := hostcfg.WriteAtomic(t.backup, before, 0o644); err != nil {
			return touchedFile{}, err
		}
		return t, nil
	}

	// 1. JSON config edits, staged in memory, written once per file.
	changed := map[string]bool{}
	wireTee := false
	for _, d := range decisions {
		if d.edit == nil {
			continue
		}
		if d.status != stWire {
			if d.carried != nil {
				newMan.Steps = append(newMan.Steps, *d.carried)
			}
			continue
		}
		doc := docs[d.edit.file]
		step := hostcfg.Step{Kind: d.edit.kind, Desc: d.edit.desc, File: d.edit.file,
			Path: d.edit.path, Installed: d.edit.value, Ident: d.edit.ident}
		switch d.edit.kind {
		case hostcfg.StepArrayAppend:
			if _, present, err := doc.ArrayLen(d.edit.path); err != nil {
				return err
			} else {
				step.CreatedKey = !present
			}
			if err := doc.ArrayAppend(d.edit.path, d.edit.value); err != nil {
				return err
			}
		case hostcfg.StepMemberSet:
			if err := doc.MemberSet(d.edit.path, d.edit.value); err != nil {
				return err
			}
			step.Prior, step.PriorAbsent = d.prior, d.priorAbsent
			if d.edit.isTee {
				wireTee = true
			}
		}
		newMan.Steps = append(newMan.Steps, step)
		changed[d.edit.file] = true
	}

	// 2. The tee's own files: the embedded script, and the delegate command it
	// hands the statusline output to.
	if wireTee {
		t, err := backup(plan.teeAsset)
		if err != nil {
			return err
		}
		files = append(files, t)
		if err := hostcfg.WriteAtomic(plan.teeAsset, statuslineTeeJS, 0o644); err != nil {
			return failWith(err)
		}
		newMan.Files = append(newMan.Files, fileRecFor(t))

		delegate := ""
		for _, d := range decisions {
			if d.edit != nil && d.edit.isTee && !d.priorAbsent {
				delegate = memberCommand(d.prior)
			}
		}
		body, err := json.MarshalIndent(map[string]string{"command": delegate}, "", "  ")
		if err != nil {
			return failWith(err)
		}
		body = append(body, '\n')
		td, err := backup(plan.delegate)
		if err != nil {
			return failWith(err)
		}
		files = append(files, td)
		if err := hostcfg.WriteAtomic(plan.delegate, body, 0o644); err != nil {
			return failWith(err)
		}
		newMan.Files = append(newMan.Files, fileRecFor(td))
	}

	for path := range changed {
		t, err := backup(path)
		if err != nil {
			return failWith(err)
		}
		// The document was parsed BEFORE the delegated steps ran, and a `claude`
		// invocation rewrites settings.json. Writing the stale in-memory copy
		// over a file that moved would silently discard whatever moved it.
		if t.existed && hostcfg.SumHex(t.before) != docs[path].LoadedSum() {
			return failWith(fmt.Errorf("%s changed while the install was running — refusing to overwrite it with a stale copy; re-run", path))
		}
		files = append(files, t)
		out := docs[path].Bytes()
		if err := hostcfg.WriteAtomic(path, out, 0o644); err != nil {
			return failWith(err)
		}
		newMan.Files = append(newMan.Files, carryOriginal(man, fileRecFor(t)))
		rep.Files = append(rep.Files, path)
	}

	// 3. Delegated registry changes, last, because they are the step most likely
	// to fail on a machine where the host CLI is missing or broken.
	for _, d := range decisions {
		if d.cli == nil {
			continue
		}
		if d.status != stWire {
			if d.carried != nil {
				newMan.Steps = append(newMan.Steps, *d.carried)
			}
			continue
		}
		env := hostEnv(f.home, d.cli.bin)
		if err := runHostCLI(d.cli.bin, d.cli.run, env); err != nil {
			return failWith(fmt.Errorf("%s: %w", d.cli.desc, err))
		}
		undoCLI = append(undoCLI, d.cli.undo)
		undoBins = append(undoBins, d.cli.bin)
		undoEnvs = append(undoEnvs, env)
		newMan.Steps = append(newMan.Steps, hostcfg.Step{Kind: hostcfg.StepHostCLI,
			Desc: d.cli.desc, Ran: append([]string{d.cli.bin}, d.cli.run...),
			Undo: append([]string{d.cli.bin}, d.cli.undo...)})
	}

	// 4. Re-hash every managed file from DISK, after the delegated steps.
	//
	// Not a nicety: a `claude` invocation of ANY kind rewrites
	// ~/.claude/settings.json as a side effect — it normalises the `model` value
	// and re-indents the document (reproduced 2026-08-11: a read-only
	// `claude mcp list` turned {"model":"opus"} into a pretty-printed
	// {"model":"opus[1m]"}). Hashing the bytes WE wrote would therefore record a
	// hash that is already stale by the time install returns, and every later
	// uninstall would see a mismatch and fall back to surgical removal — the
	// byte-identical path would be dead code on the only machine that matters.
	for i := range newMan.Files {
		cur, pres, err := hostcfg.ReadFile(newMan.Files[i].Path)
		if err != nil {
			return failWith(fmt.Errorf("re-read %s after install: %w", newMan.Files[i].Path, err))
		}
		if pres != hostcfg.PresentFile {
			return failWith(fmt.Errorf("%s vanished during install", newMan.Files[i].Path))
		}
		newMan.Files[i].SHA256After = hostcfg.SumHex(cur)
	}
	// Files an EARLIER install wrote that this run did not touch stay recorded.
	// Dropping them would leave the tee script (and its delegate) behind on the
	// next uninstall, with nothing left claiming ownership of either.
	if man != nil {
		for _, o := range man.Files {
			if priorRec := findFileRec(newMan.Files, o.Path); priorRec == nil {
				newMan.Files = append(newMan.Files, o)
			}
		}
	}

	if err := newMan.Save(manPath); err != nil {
		return failWith(fmt.Errorf("write manifest: %w", err))
	}
	return nil
}

// touchedFile is one file the installer is about to write, captured with the
// bytes it held beforehand so a rollback needs no second read.
type touchedFile struct {
	path    string
	before  []byte
	existed bool
	backup  string
}

// fileRecFor records one written file's pre-install state. SHA256After is
// filled from DISK at the end of applyInstall, never from the bytes we wrote:
// the host's own cli rewrites settings.json behind us.
func fileRecFor(t touchedFile) hostcfg.FileRec {
	r := hostcfg.FileRec{Path: t.path}
	if t.existed {
		r.Backup, r.SHA256Before = t.backup, hostcfg.SumHex(t.before)
	} else {
		r.CreatedByUs = true
	}
	return r
}

func findFileRec(recs []hostcfg.FileRec, path string) *hostcfg.FileRec {
	for i := range recs {
		if recs[i].Path == path {
			return &recs[i]
		}
	}
	return nil
}

// carryOriginal keeps the FIRST install's pre-install bytes for a file a later
// install touches again. Without this, installing a second step would record
// the already-wired file as the "original", and uninstall would restore to a
// half-installed state instead of to the operator's own config.
func carryOriginal(old *hostcfg.Manifest, r hostcfg.FileRec) hostcfg.FileRec {
	if old == nil {
		return r
	}
	for _, o := range old.Files {
		if o.Path == r.Path {
			r.Backup, r.SHA256Before, r.CreatedByUs = o.Backup, o.SHA256Before, o.CreatedByUs
			return r
		}
	}
	return r
}

// hostEnv pins the child's home so a scratch-HOME install can never write into
// the operator's real one.
func hostEnv(home, bin string) []string {
	env := []string{"HOME=" + home, "USERPROFILE=" + home}
	if bin == "codex" {
		env = append(env, "CODEX_HOME="+filepath.Join(home, ".codex"))
	}
	return env
}

// runHostCLI spawns a host CLI. B13: `claude` IS a model binary, so the
// environment is scrubbed before the deliberate home pins are appended — an
// ambient ANTHROPIC_API_KEY reaching a spawned Claude Code is metered spend
// under a receipt that reads zero.
func runHostCLI(bin string, args, extraEnv []string) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s not found on PATH: %w", bin, err)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = append(childenv.Scrub(os.Environ()), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("`%s %s` failed: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
