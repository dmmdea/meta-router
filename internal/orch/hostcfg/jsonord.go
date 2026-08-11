// Package hostcfg edits a HOST application's config file the way an installer
// must: it preserves every byte it does not own, it records what it wrote so it
// can prove ownership later, and it never removes anything it cannot prove it
// wrote.
//
// The order-preserving JSON model below exists because the obvious
// implementation is unusable in practice. Decoding a config into
// map[string]any and re-encoding it sorts every key alphabetically and
// re-indents every nested value, so a one-line hook registration rewrites the
// operator's whole hand-maintained settings.json — an unreadable diff in a file
// that is under version control on this fleet, and a needless risk in a file
// whose corruption silently disables every hook and rule.
//
// So: containers are parsed ONE LEVEL at a time and every child the caller does
// not touch is carried as its ORIGINAL bytes. Untouched members keep their
// exact formatting, their key order, their key SPELLING (an escaped é is
// not silently rewritten to é) and their scalar spelling (1.50 stays 1.50);
// only the subtree actually being edited is re-encoded.
package hostcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// style is a document's whitespace convention: one level of indentation and the
// line terminator it uses. Both are read from the source and reproduced, so a
// CRLF file does not come back with mixed endings — which on a Windows fleet
// with a git-tracked settings.json is a whole-file diff on the next commit.
type style struct {
	indent string
	nl     string
}

// member is one object entry: the decoded key, the key's ORIGINAL bytes (so an
// untouched member is re-emitted exactly as written), and the value's original
// bytes.
type member struct {
	key    string
	rawKey json.RawMessage
	val    json.RawMessage
}

// object is one level of a JSON object. Nested values stay verbatim until a
// caller parses them in turn.
type object struct {
	members []member
	index   map[string]int
}

// parseObject reads b as a single JSON object, keeping member order and each
// key's and value's original bytes.
//
// Two things it refuses rather than guesses at. Trailing content after the
// object: a config file with a second document in it is corrupt, and silently
// editing only the first half is how a truncated write becomes permanent. And
// DUPLICATE KEYS: parsers disagree about which one wins, so a document whose
// meaning is parser-dependent is one this package must not rewrite — last-wins
// would silently delete a member of the operator's file.
func parseObject(b []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object, got %v", tok)
	}
	o := &object{index: map[string]int{}}
	for dec.More() {
		start := dec.InputOffset()
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		end := dec.InputOffset()
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key, got %v", kt)
		}
		if _, dup := o.index[key]; dup {
			return nil, fmt.Errorf("duplicate key %q — refusing to rewrite a document whose meaning depends on which parser reads it", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.index[key] = len(o.members)
		o.members = append(o.members, member{key: key, rawKey: sliceKey(b, start, end, key), val: raw})
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing content after the top-level object")
	}
	return o, nil
}

// sliceKey recovers a key's ORIGINAL bytes from the source, using the decoder's
// offsets around the key token.
//
// Re-marshalling the decoded key is not equivalent: encoding/json HTML-escapes
// `<`, `>` and `&`, so an untouched member named "a<b>c&d" came back spelled
// "a<b>c&d". Rewriting members nobody asked to touch is the one
// thing this package promises not to do.
//
// The span runs from just after the previous token to just after the key, so it
// may carry a leading comma and whitespace. Anything that does not come out as
// a quoted string falls back to a re-marshal, which is always valid JSON.
func sliceKey(src []byte, start, end int64, key string) json.RawMessage {
	if start < 0 || end > int64(len(src)) || start >= end {
		return marshalKey(key)
	}
	raw := bytes.TrimSpace(src[start:end])
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte(",")))
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return marshalKey(key)
	}
	// Prove the recovered literal still means the same key before trusting it.
	var check string
	if err := json.Unmarshal(raw, &check); err != nil || check != key {
		return marshalKey(key)
	}
	return append(json.RawMessage(nil), raw...)
}

func marshalKey(key string) json.RawMessage {
	b, err := json.Marshal(key)
	if err != nil { // unreachable: Go coerces invalid UTF-8 rather than failing
		return json.RawMessage(`""`)
	}
	return b
}

// Get returns a member's raw bytes.
func (o *object) Get(key string) (json.RawMessage, bool) {
	i, ok := o.index[key]
	if !ok {
		return nil, false
	}
	return o.members[i].val, true
}

// Set replaces a member in place, or appends it when new. In place matters: an
// installer that moved `hooks` to the end of the file every run would produce a
// diff nobody reads.
func (o *object) Set(key string, raw json.RawMessage) {
	if i, ok := o.index[key]; ok {
		o.members[i].val = raw
		return
	}
	o.index[key] = len(o.members)
	o.members = append(o.members, member{key: key, rawKey: marshalKey(key), val: raw})
}

// Delete removes a member, keeping the order of the rest.
func (o *object) Delete(key string) {
	i, ok := o.index[key]
	if !ok {
		return
	}
	o.members = append(o.members[:i], o.members[i+1:]...)
	delete(o.index, key)
	for j := i; j < len(o.members); j++ {
		o.index[o.members[j].key] = j
	}
}

// Encode renders the object at nesting depth. An empty indent renders
// compactly, which is what a source document with no newlines gets back.
//
// Untouched members are emitted verbatim, so their internal indentation — which
// was written for exactly this depth — stays correct.
func (o *object) Encode(st style, depth int) []byte {
	if len(o.members) == 0 {
		return []byte("{}")
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o.members {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeNewlinePad(&buf, st, depth+1)
		buf.Write(m.rawKey)
		buf.WriteByte(':')
		if st.indent != "" {
			buf.WriteByte(' ')
		}
		buf.Write(m.val)
	}
	writeNewlinePad(&buf, st, depth)
	buf.WriteByte('}')
	return buf.Bytes()
}

// array is one level of a JSON array: element order plus each element's raw
// bytes.
type array struct{ elems []json.RawMessage }

// parseArray reads b as a single JSON array, keeping each element's original
// bytes. Like parseObject it refuses trailing content rather than editing the
// first of two documents.
func parseArray(b []byte) (*array, error) {
	var elems []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&elems); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing content after the array")
	}
	return &array{elems: elems}, nil
}

// Len reports the element count.
func (a *array) Len() int { return len(a.elems) }

// At returns one element's raw bytes.
func (a *array) At(i int) json.RawMessage { return a.elems[i] }

// Append adds an element at the end.
func (a *array) Append(raw json.RawMessage) { a.elems = append(a.elems, raw) }

// RemoveAt drops the element at i.
func (a *array) RemoveAt(i int) { a.elems = append(a.elems[:i], a.elems[i+1:]...) }

// Encode renders the array at nesting depth. See object.Encode.
func (a *array) Encode(st style, depth int) []byte {
	if len(a.elems) == 0 {
		return []byte("[]")
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range a.elems {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeNewlinePad(&buf, st, depth+1)
		buf.Write(e)
	}
	writeNewlinePad(&buf, st, depth)
	buf.WriteByte(']')
	return buf.Bytes()
}

func writeNewlinePad(buf *bytes.Buffer, st style, depth int) {
	if st.indent == "" {
		return
	}
	buf.WriteString(st.nl)
	for i := 0; i < depth; i++ {
		buf.WriteString(st.indent)
	}
}

// detectStyle reads a document's indentation unit and line terminator.
//
// The unit comes from the first line that is actually INDENTED, not merely the
// line after the first break. A blank line before the first member — ordinary
// in a hand-maintained file — made the earlier version report "no indentation",
// which then flattened the whole document into one line while untouched nested
// values kept their pretty-printed bytes: valid JSON, unreadable diff, the
// precise outcome this package exists to prevent.
func detectStyle(b []byte) style {
	st := style{nl: "\n"}
	if bytes.Contains(b, []byte("\r\n")) {
		st.nl = "\r\n"
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		n := i + 1
		for n < len(b) && (b[n] == ' ' || b[n] == '\t') {
			n++
		}
		if n == i+1 { // the line starts flush left: blank line, or a closing brace
			continue
		}
		if n < len(b) && b[n] != '\r' && b[n] != '\n' {
			st.indent = string(b[i+1 : n])
			return st
		}
	}
	return st
}

// canon returns a canonical form of a JSON value for COMPARISON only: object
// keys sorted, insignificant whitespace gone. Ownership checks must compare
// meaning, not spelling — an operator who re-indents settings.json or whose
// editor reorders a hook's fields has not taken ownership of the entry, and
// refusing to uninstall over a whitespace change would be a false drift.
func canon(raw json.RawMessage) (string, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SameValue reports whether two JSON values mean the same thing. An unparseable
// side is never "the same": it is reported as an error so the caller can refuse
// rather than guess.
func SameValue(a, b json.RawMessage) (bool, error) {
	ca, err := canon(a)
	if err != nil {
		return false, err
	}
	cb, err := canon(b)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}
