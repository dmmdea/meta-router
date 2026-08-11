package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/dmmdea/meta-router/internal/orch/hostcfg"
)

// restore modes, reported so the operator is never told "restored" when only
// the semantics — and not the bytes — came back.
const (
	restoreByte     = "byte-identical"
	restoreSurgical = "surgical"
	restoreNone     = "nothing-to-restore"
)

// undoStep is one recorded step plus what uninstall decided to do with it.
type undoStep struct {
	step     *hostcfg.Step
	status   string
	detail   string
	index    int  // array element to drop
	modified bool // still ours, but edited since install
}

// runUninstall reverses exactly what the manifest records, and nothing else.
//
// Two outcomes are possible and they are NOT the same thing, so both are
// reported by name. When a config file still holds the exact bytes this
// installer left, the pre-install copy is restored verbatim — byte-identical,
// including whatever formatting and key order the operator had. When the file
// has changed since (the operator edited some other part of it), restoring that
// copy would silently discard their work, so the recorded entries are removed
// surgically instead and the result is reported as `surgical`.
func runUninstall(args []string) error {
	f, err := parseInstallArgs("uninstall", args)
	if err != nil {
		return err
	}
	stateDir := f.stateDir
	// Reject an unknown host BEFORE anything else. Checking it after the
	// no-manifest early return made `uninstall emacs` report a tidy success
	// having understood nothing — a typo'd host name would have read as "there
	// was nothing to remove".
	if _, err := planFor(f.host, f.home, f.binDir, stateDir); err != nil {
		return err
	}
	manPath := hostcfg.ManifestPath(stateDir, f.host)
	man, p, err := hostcfg.LoadManifest(manPath)
	if err != nil {
		return err
	}
	rep := &installReport{Host: f.host, Action: "uninstall", DryRun: f.dryRun,
		BinDir: f.binDir, Manifest: manPath}
	if p == hostcfg.AbsentFile {
		rep.Result, rep.Restore = "ok", restoreNone
		rep.Note = "no install manifest for this host — mr-orchestrate has nothing recorded to remove. " +
			"Entries wired by hand are deliberately left alone."
		emitReport(rep, f.asJSON)
		return nil
	}

	docs := map[string]*hostcfg.Doc{}
	for i := range man.Steps {
		s := &man.Steps[i]
		if s.File == "" {
			continue
		}
		if _, ok := docs[s.File]; ok {
			continue
		}
		d, _, err := hostcfg.LoadDoc(s.File)
		if err != nil {
			return fmt.Errorf("read host config: %w", err)
		}
		docs[s.File] = d
	}

	plan, err := planFor(f.host, f.home, f.binDir, stateDir)
	if err != nil {
		return err
	}
	undos, refuse, err := assessUninstall(man, docs, plan)
	if err != nil {
		return err
	}
	// A file gets its bytes back only when it still holds exactly what we left
	// AND every entry we are removing from it is unmodified.
	// A byte restore also needs a USABLE pre-install copy. Verifying that here
	// rather than at write time is what keeps a bad backup from wedging the
	// command: an unusable copy now downgrades that file to surgical removal,
	// where it used to abort the whole uninstall with everything still wired and
	// no report printed — permanently, on every retry.
	byteOK := map[string]bool{}
	for _, fr := range man.Files {
		cur, pres, err := hostcfg.ReadFile(fr.Path)
		if err != nil {
			return fmt.Errorf("read %s: %w", fr.Path, err)
		}
		unchanged := pres == hostcfg.PresentFile && hostcfg.SumHex(cur) == fr.SHA256After
		byteOK[fr.Path] = unchanged && backupUsable(fr)
	}
	for _, u := range undos {
		if u.modified && u.step.File != "" {
			byteOK[u.step.File] = false
		}
	}

	for _, u := range undos {
		rep.Steps = append(rep.Steps, stepReport{Desc: u.step.Desc, Kind: u.step.Kind,
			Status: u.status, Detail: u.detail})
	}
	if refuse {
		rep.Result, rep.Restore = "refused", restoreNone
		rep.Note = refusalNote("uninstall", f.host, manPath, true, false)
		emitReport(rep, f.asJSON)
		return fmt.Errorf("uninstall refused: the host config no longer matches what was installed")
	}

	mode := restoreByte
	for _, fr := range man.Files {
		if !byteOK[fr.Path] {
			mode = restoreSurgical
			break
		}
	}
	if len(man.Files) == 0 {
		mode = restoreNone
	}
	rep.Restore = mode
	if f.dryRun {
		rep.Result = "ok"
		rep.Note = "dry run: nothing was written. " + restoreNote(mode)
		emitReport(rep, f.asJSON)
		return nil
	}

	// 1. Reverse the delegated registry changes first: they are the step most
	// likely to fail, and failing before we have touched any file leaves the
	// install intact rather than half-removed.
	for _, u := range undos {
		if u.step.Kind != hostcfg.StepHostCLI || u.status != stRemove {
			continue
		}
		bin := u.step.Undo[0]
		if err := runHostCLI(bin, u.step.Undo[1:], hostEnv(f.home, bin)); err != nil {
			return err
		}
	}

	// 2. Files whose bytes we can hand back verbatim.
	for _, fr := range man.Files {
		if !byteOK[fr.Path] {
			continue
		}
		if fr.CreatedByUs {
			if err := os.Remove(fr.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
			rep.Files = append(rep.Files, fr.Path+" (removed — created by install)")
			continue
		}
		before, _, err := hostcfg.ReadFile(fr.Backup) // byteOK already proved it usable
		if err != nil {
			return fmt.Errorf("read backup %s: %w", fr.Backup, err)
		}
		if err := hostcfg.WriteAtomic(fr.Path, before, 0o644); err != nil {
			return err
		}
		rep.Files = append(rep.Files, fr.Path+" (restored byte-identical)")
	}

	// 3. Files that changed since install: remove our entries, keep theirs.
	surgical := map[string]bool{}
	for _, u := range undos {
		if u.step.File == "" || u.status != stRemove || byteOK[u.step.File] {
			continue
		}
		doc := docs[u.step.File]
		switch u.step.Kind {
		case hostcfg.StepArrayAppend:
			// Re-locate by ident RIGHT NOW rather than trusting the index taken
			// during assessment. Two recorded steps on one array path would
			// otherwise remove element 0, shifting element 1 down, and the second
			// removal would delete the operator's NEXT entry — the exact
			// "delete something it did not write" outcome this command forbids.
			idx, amb, undetermined, err := findOurs(doc, u.step.Path, u.step.Ident)
			if err != nil {
				return err
			}
			if idx < 0 || amb || undetermined {
				return fmt.Errorf("%s: %v is no longer in a state this uninstall can safely edit (found=%d ambiguous=%v undetermined=%v)",
					u.step.File, u.step.Path, idx, amb, undetermined)
			}
			if err := doc.ArrayRemoveAt(u.step.Path, idx); err != nil {
				return err
			}
			n, _, err := doc.ArrayLen(u.step.Path)
			if err != nil {
				return err
			}
			if n == 0 && u.step.CreatedKey {
				if err := doc.MemberDelete(u.step.Path); err != nil {
					return err
				}
			}
		case hostcfg.StepMemberSet:
			if u.step.PriorAbsent {
				if err := doc.MemberDelete(u.step.Path); err != nil {
					return err
				}
			} else if err := doc.MemberSet(u.step.Path, u.step.Prior); err != nil {
				return err
			}
		}
		surgical[u.step.File] = true
	}
	for path := range surgical {
		if err := docs[path].Save(); err != nil {
			return err
		}
		rep.Files = append(rep.Files, path+" (entries removed; the file had changed since install)")
	}
	for _, fr := range man.Files {
		if byteOK[fr.Path] || surgical[fr.Path] {
			continue
		}
		rep.Files = append(rep.Files, fr.Path+" (LEFT IN PLACE — changed since install, and it holds no removable entry)")
	}

	if err := hostcfg.RemoveManifest(stateDir, f.host); err != nil {
		return err
	}
	rep.Result = "ok"
	rep.Note = restoreNote(mode)
	emitReport(rep, f.asJSON)
	return nil
}

// backupUsable reports whether a file's pre-install copy can still be trusted
// to be exactly what was there before the install.
func backupUsable(fr hostcfg.FileRec) bool {
	if fr.CreatedByUs {
		return true // nothing to restore: the file did not exist, so removal IS the restore
	}
	if fr.Backup == "" || fr.SHA256Before == "" {
		return false
	}
	b, p, err := hostcfg.ReadFile(fr.Backup)
	return err == nil && p == hostcfg.PresentFile && hostcfg.SumHex(b) == fr.SHA256Before
}

func restoreNote(mode string) string {
	switch mode {
	case restoreByte:
		return "every managed file still held exactly what install left, so the pre-install copies were restored byte for byte."
	case restoreSurgical:
		return "at least one managed file had changed since install, so its own edits were kept and only the recorded entries were removed — the result is semantically clean, not byte-identical."
	default:
		return "no managed files were recorded."
	}
}

// cliStepFor finds the plan's delegated step matching a recorded one, so
// uninstall can reuse its read-only probe.
func cliStepFor(plan *hostPlan, desc string) *cliStep {
	for i := range plan.clis {
		if plan.clis[i].desc == desc {
			return &plan.clis[i]
		}
	}
	return nil
}

// assessUninstall decides each recorded step WITHOUT writing anything.
func assessUninstall(man *hostcfg.Manifest, docs map[string]*hostcfg.Doc, plan *hostPlan) ([]undoStep, bool, error) {
	var out []undoStep
	refuse := false
	for i := range man.Steps {
		s := &man.Steps[i]
		u := undoStep{step: s, index: -1}
		switch s.Kind {
		case hostcfg.StepArrayAppend:
			d := docs[s.File]
			idx, amb, undetermined, err := findOurs(d, s.Path, s.Ident)
			if err != nil {
				return nil, false, err
			}
			switch {
			case amb:
				u.status, u.detail, refuse = stDrift,
					fmt.Sprintf("two entries in %s invoke %s — removing one would be a coin flip", strings.Join(s.Path, "."), s.Ident), true
			case undetermined:
				// The dangerous direction of absent-vs-undetermined. Reading an
				// unparseable entry as "already gone" made uninstall report OK,
				// delete the manifest, and leave our hook wired with nothing left
				// on disk claiming ownership of it.
				u.status, u.detail, refuse = stDrift,
					fmt.Sprintf("an entry in %s could not be parsed, so it is unknown whether ours is still there — that is not the same as gone", strings.Join(s.Path, ".")), true
			case idx < 0:
				u.status, u.detail = stNotWired, "already gone from the host config"
			default:
				cur, err := d.ArrayAt(s.Path, idx)
				if err != nil {
					return nil, false, err
				}
				same, err := hostcfg.SameValue(cur, s.Installed)
				u.status, u.index = stRemove, idx
				if err != nil || !same {
					u.modified = true
					u.detail = "edited since install; the entry is still ours to remove, but the file cannot be restored byte-identically"
				}
			}
		case hostcfg.StepMemberSet:
			d := docs[s.File]
			cur, present, err := d.MemberGet(s.Path)
			if err != nil {
				return nil, false, err
			}
			switch {
			case !present:
				u.status, u.detail = stNotWired, "already gone from the host config"
			case !strings.Contains(memberCommand(cur), s.Ident):
				u.status, u.detail, refuse = stDrift,
					fmt.Sprintf("%s now holds something that is not ours — restoring over it would destroy an operator setting", strings.Join(s.Path, ".")), true
			default:
				u.status = stRemove
				if same, err := hostcfg.SameValue(cur, s.Installed); err != nil || !same {
					u.modified = true
					u.detail = "edited since install; still ours to remove, but not a byte-identical restore"
				}
			}
		case hostcfg.StepHostCLI:
			if len(s.Undo) < 2 {
				u.status, u.detail, refuse = stDrift, "manifest records no undo command for this step", true
				break
			}
			// PROBE before acting, exactly as install does. Without it, a server
			// the operator had already removed by hand made `claude mcp remove`
			// exit 1, which aborted the whole uninstall with the config still
			// fully wired — and every retry hit the same line.
			u.status = stRemove
			if c := cliStepFor(plan, s.Desc); c != nil {
				p, _, err := c.probe()
				switch {
				case err != nil:
					u.status, u.detail, refuse = stDrift,
						fmt.Sprintf("could not read the host's MCP registry: %v", err), true
				case p == hostcfg.AbsentFile:
					u.status, u.detail = stNotWired, "already deregistered from the host"
				}
			}
		default:
			u.status, u.detail, refuse = stDrift, "unknown step kind "+s.Kind, true
		}
		out = append(out, u)
	}
	return out, refuse, nil
}
