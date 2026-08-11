package hostcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ManifestSchema is bumped when the recorded shape changes incompatibly. An
// uninstall that meets a schema it does not understand REFUSES rather than
// guessing at a layout — the whole value of the manifest is that it is the only
// proof of what we may remove.
const ManifestSchema = 1

// Step kinds. Each names both what was done and how to reverse it.
const (
	// StepArrayAppend: one element was appended to a JSON array (a hook entry).
	StepArrayAppend = "array_append"
	// StepMemberSet: one object member was written, replacing Prior (or adding
	// it where PriorAbsent).
	StepMemberSet = "member_set"
	// StepHostCLI: the host's OWN cli was asked to make the change, because the
	// file behind it is live state the host writes concurrently and a
	// second-process read-modify-write would drop whatever it wrote in between.
	StepHostCLI = "host_cli"
)

// Step is one reversible unit of an install.
type Step struct {
	Kind string `json:"kind"`
	Desc string `json:"desc"`

	// JSON-edit steps.
	File        string          `json:"file,omitempty"`
	Path        []string        `json:"path,omitempty"`
	Installed   json.RawMessage `json:"installed,omitempty"`
	Prior       json.RawMessage `json:"prior,omitempty"`
	PriorAbsent bool            `json:"prior_absent,omitempty"`
	// Ident is the command substring that identifies OUR entry among its
	// neighbours after the operator may have re-ordered or tuned the list.
	// Recorded rather than re-derived so an uninstall never has to guess which
	// of several entries it wrote.
	Ident string `json:"ident,omitempty"`
	// CreatedKey: the array (or member) did not exist before this step, so a
	// surgical removal that empties it should take the key with it rather than
	// leaving `"UserPromptSubmit": []` behind in an operator's config.
	CreatedKey bool `json:"created_key,omitempty"`

	// Host-CLI steps.
	Ran  []string `json:"ran,omitempty"`
	Undo []string `json:"undo,omitempty"`
}

// FileRec is one file the installer wrote, with the hashes that decide whether
// a byte-identical restore is still safe.
type FileRec struct {
	Path string `json:"path"`
	// Backup holds the file's bytes as they were BEFORE the install. Empty when
	// the file did not exist.
	Backup string `json:"backup,omitempty"`
	// SHA256Before is empty when the file did not exist before the install.
	SHA256Before string `json:"sha256_before,omitempty"`
	// SHA256After is what we left on disk. An uninstall compares against it to
	// tell "nobody has touched this since" from "the operator edited it".
	SHA256After string `json:"sha256_after"`
	// CreatedByUs: the file did not exist before the install, so a clean
	// uninstall deletes it rather than leaving an empty husk.
	CreatedByUs bool `json:"created_by_us,omitempty"`
}

// Manifest is the record of one host install: the ONLY authority for what
// mr-orchestrate may later remove.
//
// Ownership is deliberately not inferred. An entry that merely LOOKS like ours
// — same command, same shape — but is absent from the manifest was written by
// someone else (a hand edit, another tool, an older uninstalled run), and the
// installer refuses to adopt or delete it. Guessing here is how an installer
// deletes an operator's own hook and calls it cleanup.
type Manifest struct {
	Schema      int       `json:"schema"`
	Host        string    `json:"host"`
	MRVersion   string    `json:"mr_version"`
	InstalledAt string    `json:"installed_at"`
	BinDir      string    `json:"bin_dir"`
	Files       []FileRec `json:"files"`
	Steps       []Step    `json:"steps"`
}

// ManifestPath is where a host's install record lives.
func ManifestPath(stateDir, host string) string {
	return filepath.Join(stateDir, "install", host+".json")
}

// BackupPath is where a host install stashes one file's pre-install bytes.
func BackupPath(stateDir, host, name string) string {
	return filepath.Join(stateDir, "install", host, name+".pre-install")
}

// LoadManifest reads a host's install record. Absent means "never installed by
// us"; unreadable is an ERROR, because treating an unreadable manifest as
// absent would let an install proceed as if nothing were wired and an uninstall
// report success having removed nothing.
func LoadManifest(path string) (*Manifest, Presence, error) {
	b, p, err := ReadFile(path)
	if err != nil || p == AbsentFile {
		return nil, p, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, PresentFile, fmt.Errorf("%s: %w", path, err)
	}
	if m.Schema != ManifestSchema {
		return nil, PresentFile, fmt.Errorf(
			"%s: manifest schema %d, this binary understands %d — refusing to act on a record it cannot read",
			path, m.Schema, ManifestSchema)
	}
	for i, s := range m.Steps {
		if err := s.validate(); err != nil {
			return nil, PresentFile, fmt.Errorf("%s: step %d (%s): %w — refusing to act on a record it cannot trust",
				path, i, s.Kind, err)
		}
	}
	return &m, PresentFile, nil
}

// validate rejects a step that cannot be reversed safely.
//
// This file is UNTRUSTED INPUT by design: it lives in a state dir, survives
// version changes, and the refusal message tells operators to delete or edit it.
// A schema check alone was not enough. A `member_set` step with an empty Ident
// walked straight through uninstall, because `strings.Contains(anything, "")`
// is true — so any operator statusLine read as ours, and the restore then wrote
// the step's absent Prior into the file. The uninstaller would have corrupted
// the config it exists to protect. A step missing File would panic on a nil
// document instead.
func (s Step) validate() error {
	switch s.Kind {
	case StepArrayAppend, StepMemberSet:
		if s.File == "" || len(s.Path) == 0 || s.Ident == "" {
			return errors.New("needs file, path and ident")
		}
		if s.Kind == StepArrayAppend && !json.Valid(s.Installed) {
			return errors.New("installed value is not valid JSON")
		}
		if s.Kind == StepMemberSet {
			if !json.Valid(s.Installed) {
				return errors.New("installed value is not valid JSON")
			}
			if !s.PriorAbsent && !json.Valid(s.Prior) {
				return errors.New("prior value is neither absent nor valid JSON")
			}
		}
	case StepHostCLI:
		if len(s.Ran) < 1 || len(s.Undo) < 2 {
			return errors.New("needs the command it ran and a reversal of at least a binary plus one argument")
		}
	default:
		return errors.New("unknown step kind")
	}
	return nil
}

// Save writes the manifest atomically.
func (m *Manifest) Save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(b, '\n'), 0o644)
}

// RemoveManifest deletes a host's install record and its backup directory.
func RemoveManifest(stateDir, host string) error {
	if err := os.Remove(ManifestPath(stateDir, host)); err != nil && !os.IsNotExist(err) {
		return err
	}
	err := os.RemoveAll(filepath.Join(stateDir, "install", host))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
