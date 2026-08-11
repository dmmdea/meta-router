package hostcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Presence is the three-valued answer to "is this config file there?".
//
// The third value is the point. A file we could not READ — permission denied, a
// locked handle, an unreachable UNC path — is NOT an absent file, and code that
// collapses the two converts a transient fault into a permanent one: an
// installer would "helpfully" create a fresh config over a file it simply could
// not open, and an uninstaller would report a clean removal it never performed.
type Presence int

const (
	// UnknownFile: the read failed for any reason other than absence. Never
	// destructive.
	//
	// It is FIRST deliberately, so it is the zero value. An unset field, a map
	// miss, or a `return 0, err` on some future path then reads as "I could not
	// tell" rather than as "the file is there and I read it fine" — the one
	// reading that gets a config file deleted.
	UnknownFile Presence = iota
	// PresentFile: read succeeded.
	PresentFile
	// AbsentFile: the filesystem said it does not exist.
	AbsentFile
)

// ReadFile reads path, distinguishing "not there" from "could not tell".
func ReadFile(path string) ([]byte, Presence, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return b, PresentFile, nil
	case os.IsNotExist(err):
		return nil, AbsentFile, nil
	default:
		return nil, UnknownFile, err
	}
}

// SumHex is the content hash used for "has this file changed since we wrote it".
func SumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// WriteAtomic replaces path via a temp file in the SAME directory plus a
// rename, so a config file is never observed half-written.
//
// This is not a theoretical concern on this fleet: a settings.json write that
// SHRANK the file once left the old tail in place, producing invalid JSON that
// silently made every rule and hook inert. A rename cannot produce that state.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeded
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Doc is a JSON config file held for editing: its root object, the indentation
// unit detected from the source, and the original bytes.
type Doc struct {
	path       string
	st         style
	root       *object
	trailingNL bool
	// bom: the source began with a UTF-8 byte-order mark, which PowerShell 5.1's
	// Set-Content writes by default — so this is a live-fleet state, not a
	// contrived one. It is stripped before parsing (the decoder rejects it with
	// an opaque `invalid character 'ï'`) and written back on save.
	bom bool
	// loaded is the hash of the bytes this document was parsed from, so a write
	// can refuse when the file moved underneath it.
	loaded string
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// LoadDoc reads a JSON config file. An absent file yields an empty document, so
// a host with no config yet can still be wired. An UNREADABLE file is an error
// — never an empty document, which would have the installer build a fresh
// config over a file it merely failed to open.
func LoadDoc(path string) (*Doc, Presence, error) {
	b, p, err := ReadFile(path)
	if err != nil {
		return nil, p, err
	}
	if p == AbsentFile {
		return &Doc{path: path, st: style{indent: "  ", nl: "\n"}, root: &object{index: map[string]int{}},
			trailingNL: true}, p, nil
	}
	loaded := SumHex(b)
	bom := bytes.HasPrefix(b, utf8BOM)
	body := b
	if bom {
		body = b[len(utf8BOM):]
	}
	root, err := parseObject(body)
	if err != nil {
		return nil, PresentFile, fmt.Errorf("%s: %w", path, err)
	}
	return &Doc{path: path, st: detectStyle(body), root: root, bom: bom, loaded: loaded,
		trailingNL: bytes.HasSuffix(body, []byte("\n"))}, PresentFile, nil
}

// Bytes renders the document, restoring the source's byte-order mark and
// trailing newline.
func (d *Doc) Bytes() []byte {
	var out []byte
	if d.bom {
		out = append(out, utf8BOM...)
	}
	out = append(out, d.root.Encode(d.st, 0)...)
	if d.trailingNL {
		out = append(out, d.st.nl...)
	}
	return out
}

// LoadedSum is the hash of the bytes this document was parsed from, so a caller
// can refuse to write over a file that moved in between.
func (d *Doc) LoadedSum() string { return d.loaded }

// Save writes the rendered document atomically.
func (d *Doc) Save() error { return WriteAtomic(d.path, d.Bytes(), 0o644) }

// chain resolves path[:len(path)-1] as a chain of nested objects, root first.
// With create set, missing intermediate objects are added; without it, a
// missing link returns a nil chain and no error — "the key is not there" is a
// normal answer, not a failure.
func (d *Doc) chain(path []string, create bool) ([]*object, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("empty config path")
	}
	objs := []*object{d.root}
	for i := 0; i < len(path)-1; i++ {
		parent := objs[i]
		raw, ok := parent.Get(path[i])
		if !ok {
			if !create {
				return nil, nil
			}
			raw = json.RawMessage("{}")
		}
		child, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an object: %w", d.path, path[i], err)
		}
		objs = append(objs, child)
	}
	return objs, nil
}

// writeBack re-encodes the chain from the leaf upward, each level at its own
// depth, so nested edits land with the indentation the file already uses.
func (d *Doc) writeBack(objs []*object, path []string) {
	for i := len(objs) - 1; i >= 1; i-- {
		objs[i-1].Set(path[i-1], objs[i].Encode(d.st, i))
	}
}

// MemberGet returns the value at path, and whether it is present.
func (d *Doc) MemberGet(path []string) (json.RawMessage, bool, error) {
	objs, err := d.chain(path, false)
	if err != nil || objs == nil {
		return nil, false, err
	}
	raw, ok := objs[len(objs)-1].Get(path[len(path)-1])
	return raw, ok, nil
}

// MemberSet writes value at path, creating intermediate objects as needed.
func (d *Doc) MemberSet(path []string, value json.RawMessage) error {
	if err := checkValue(value); err != nil {
		return fmt.Errorf("%s: value for %v: %w", d.path, path, err)
	}
	objs, err := d.chain(path, true)
	if err != nil {
		return err
	}
	leafDepth := len(objs) - 1
	objs[leafDepth].Set(path[len(path)-1], reindent(value, d.st, leafDepth+1))
	d.writeBack(objs, path)
	return nil
}

// checkValue refuses to store anything that is not a JSON value.
//
// Without it the document silently renders as invalid JSON and Save returns
// nil: reindent falls back to its input when json.Indent fails, object.Set
// stores it, and Encode writes it verbatim — so `MemberSet(path, nil)` produced
// `"statusLine": ` with no value and reported success. That is precisely the
// "settings.json invalid ⇒ every rule and hook silently inert" outcome this
// package exists to prevent, arriving through the package itself.
func checkValue(v json.RawMessage) error {
	if len(v) == 0 {
		return errors.New("empty (a missing value is not a JSON null)")
	}
	if !json.Valid(v) {
		return errors.New("not valid JSON")
	}
	return nil
}

// MemberDelete removes the value at path. A missing key is not an error: the
// caller's goal — "this key is not in the file" — is already true.
func (d *Doc) MemberDelete(path []string) error {
	objs, err := d.chain(path, false)
	if err != nil || objs == nil {
		return err
	}
	objs[len(objs)-1].Delete(path[len(path)-1])
	d.writeBack(objs, path)
	return nil
}

// arrayAt loads the array at path. A missing key yields an empty array with
// present=false so callers can tell "no such list" from "an empty list".
func (d *Doc) arrayAt(path []string, create bool) ([]*object, *array, bool, error) {
	objs, err := d.chain(path, create)
	if err != nil {
		return nil, nil, false, err
	}
	if objs == nil {
		return nil, &array{}, false, nil
	}
	raw, ok := objs[len(objs)-1].Get(path[len(path)-1])
	if !ok {
		return objs, &array{}, false, nil
	}
	arr, err := parseArray(raw)
	if err != nil {
		return nil, nil, false, fmt.Errorf("%s: %q is not an array: %w",
			d.path, path[len(path)-1], err)
	}
	return objs, arr, true, nil
}

// ArrayAt returns element i's raw bytes.
func (d *Doc) ArrayAt(path []string, i int) (json.RawMessage, error) {
	_, arr, present, err := d.arrayAt(path, false)
	if err != nil {
		return nil, err
	}
	if !present || i < 0 || i >= arr.Len() {
		return nil, fmt.Errorf("%s: no element %d at %v", d.path, i, path)
	}
	return arr.At(i), nil
}

// ArrayAppend adds elem to the end of the array at path, creating the array
// and any intermediate objects when absent.
func (d *Doc) ArrayAppend(path []string, elem json.RawMessage) error {
	if err := checkValue(elem); err != nil {
		return fmt.Errorf("%s: element for %v: %w", d.path, path, err)
	}
	objs, arr, _, err := d.arrayAt(path, true)
	if err != nil {
		return err
	}
	leafDepth := len(objs) - 1
	arr.Append(reindent(elem, d.st, leafDepth+2))
	objs[leafDepth].Set(path[len(path)-1], arr.Encode(d.st, leafDepth+1))
	d.writeBack(objs, path)
	return nil
}

// ArrayRemoveAt drops one element, and drops the whole array key when that
// leaves it empty ONLY if we also created it — see RemoveEmptyArray.
func (d *Doc) ArrayRemoveAt(path []string, i int) error {
	objs, arr, present, err := d.arrayAt(path, false)
	if err != nil {
		return err
	}
	if !present || i < 0 || i >= arr.Len() {
		return fmt.Errorf("%s: no element %d at %v to remove", d.path, i, path)
	}
	arr.RemoveAt(i)
	leafDepth := len(objs) - 1
	objs[leafDepth].Set(path[len(path)-1], arr.Encode(d.st, leafDepth+1))
	d.writeBack(objs, path)
	return nil
}

// ArrayLen reports the array's length, and whether the key exists at all.
func (d *Doc) ArrayLen(path []string) (int, bool, error) {
	_, arr, present, err := d.arrayAt(path, false)
	if err != nil {
		return 0, false, err
	}
	return arr.Len(), present, nil
}

// reindent re-encodes a value so its internal line breaks carry the file's own
// indentation at the depth it is being written to. Values built in code are
// compact, and dropping a compact blob into a pretty-printed file is the kind
// of cosmetic damage that makes an operator distrust an installer.
func reindent(v json.RawMessage, st style, depth int) json.RawMessage {
	if st.indent == "" {
		var compact bytes.Buffer
		if err := json.Compact(&compact, v); err != nil {
			return v // unreachable: callers pass checkValue-gated values
		}
		return compact.Bytes()
	}
	var out bytes.Buffer
	prefix := ""
	for i := 0; i < depth; i++ {
		prefix += st.indent
	}
	if err := json.Indent(&out, v, prefix, st.indent); err != nil {
		return v // unreachable: see above
	}
	b := out.Bytes()
	if st.nl != "\n" {
		b = bytes.ReplaceAll(b, []byte("\n"), []byte(st.nl))
	}
	return b
}
