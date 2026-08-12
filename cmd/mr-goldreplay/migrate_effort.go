package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dmmdea/meta-router/internal/policyeval"
)

// Effort joined the evidence cell after 825 oracle rows had already been
// recorded without it. Those rows are not worthless and they are not evidence
// about any effort either: they are observations at an UNRECORDED effort, which
// is a real config value of its own (B6 — unknown cells are counted, never
// imputed). This migration says that out loud in the data, so a legacy row can
// never be silently credited to a rank-table entry that names an effort.
//
// It is a BYTE SPLICE, not a decode-and-re-marshal. Re-marshalling through a Go
// struct would drop any field the struct does not know about, reorder keys, and
// round every integer through float64 — the exact lossy-re-encode class this
// repo closed in v0.27.1. Splicing touches only the bytes it must.

// migrateEffort stamps policyeval.EffortUnrecorded on every JSONL row whose
// top-level `effort` is absent or blank, and returns the rewritten bytes plus
// the number of rows changed.
//
// It is a FIXED POINT: a second pass reports 0 changed and returns
// byte-identical output. Anything else churns the live oracle (and its git
// history) on every run.
func migrateEffort(in []byte) (out []byte, changed int, err error) {
	var b bytes.Buffer
	b.Grow(len(in) + len(in)/16)
	lines := bytes.Split(in, []byte("\n"))
	for i, line := range lines {
		// bytes.Split leaves a trailing empty element for a newline-terminated
		// file; re-emitting it verbatim preserves the exact terminator.
		if i > 0 {
			b.WriteByte('\n')
		}
		stamped, did, serr := stampLine(line)
		if serr != nil {
			return nil, 0, fmt.Errorf("line %d: %w", i+1, serr)
		}
		if did {
			changed++
		}
		b.Write(stamped)
	}
	return b.Bytes(), changed, nil
}

// stampLine rewrites one line if it is a JSON object needing the stamp. A line
// that is blank, or that does not parse as a JSON object, is returned VERBATIM:
// loadDone already tolerates a torn line, and a migration that drops or
// rewrites bytes it cannot read is how evidence disappears.
func stampLine(line []byte) ([]byte, bool, error) {
	// A CR belongs to the line terminator, not the JSON — hold it aside so a
	// CRLF file round-trips exactly.
	cr := []byte(nil)
	body := line
	if n := len(body); n > 0 && body[n-1] == '\r' {
		cr, body = body[n-1:], body[:n-1]
	}
	if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return line, false, nil
	}
	// A line that does not fully parse is returned VERBATIM. Without this the
	// default branch below spliced at the last '}' — which in a TRUNCATED
	// line can sit inside a completed string value, so the one function that
	// promises not to rewrite what it cannot read corrupted it (an
	// interrupted append-only replay is exactly how a torn line arises, and
	// notes routinely contain '}').
	if !json.Valid(body) {
		return line, false, nil
	}
	start, end, found, err := effortValueSpan(body)
	if err != nil {
		return line, false, nil // unparseable: verbatim, never an error
	}
	stamp := []byte(`"` + policyeval.EffortUnrecorded + `"`)
	var outBody []byte
	switch {
	case found:
		var cur string
		if json.Unmarshal(body[start:end], &cur) == nil && cur != "" {
			return line, false, nil // already pinned — byte-identical
		}
		// A NON-STRING effort (3, null, an object) is unexpected data, not a
		// blank to fill. Overwriting it destroys the only copy in a file whose
		// own header calls it unreproducible without re-spending every
		// dispatch. Refuse the line and let the operator look.
		if !bytes.Equal(bytes.TrimSpace(body[start:end]), []byte(`""`)) {
			var probe any
			if json.Unmarshal(body[start:end], &probe) == nil {
				if _, isStr := probe.(string); !isStr {
					return line, false, fmt.Errorf("effort is %s, not a string: refusing to overwrite it", bytes.TrimSpace(body[start:end]))
				}
			}
		}
		outBody = append(append(append([]byte{}, body[:start]...), stamp...), body[end:]...)
	default:
		close := bytes.LastIndexByte(body, '}')
		if close < 0 {
			return line, false, nil
		}
		member := append([]byte(`,"effort":`), stamp...)
		if open := bytes.IndexByte(body, '{'); open >= 0 && len(bytes.TrimSpace(body[open+1:close])) == 0 {
			member = member[1:] // an empty object takes no leading comma
		}
		outBody = append(append(append([]byte{}, body[:close]...), member...), body[close:]...)
	}
	return append(outBody, cr...), true, nil
}

// effortValueSpan locates the top-level `effort` member's VALUE inside a JSON
// object, as a byte span into line. found=false means the key is absent.
//
// It walks tokens rather than matching text because `"effort"` can legitimately
// appear inside another member's string value (a `note`, say), and a textual
// match would splice the stamp into the middle of that prose.
func effortValueSpan(line []byte) (start, end int, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, fmt.Errorf("not a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		key, _ := kt.(string)
		afterKey := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		afterValue := dec.InputOffset()
		if key != "effort" {
			continue
		}
		// Skip the ':' and any whitespace between the key and its value so the
		// span is the value alone.
		s := int(afterKey)
		for s < int(afterValue) {
			switch line[s] {
			case ':', ' ', '\t', '\r', '\n':
				s++
				continue
			}
			break
		}
		return s, int(afterValue), true, nil
	}
	return 0, 0, false, nil
}

// migrateEffortFile applies migrateEffort to a file via write-temp-then-rename.
// Never an in-place truncation: a process death mid-write would take the whole
// oracle with it, and the oracle is not reproducible without re-spending every
// dispatch that built it.
// looksLikeOracle gates the rewrite on the file actually BEING an oracle.
// Without it a mistyped path stamped "effort" into every record of whatever
// JSONL it was handed — dispatch.jsonl and quota-trace.jsonl sit one
// directory away — and reported success. Every non-blank line must carry the
// row identity (task + lane); a file with none is refused.
func looksLikeOracle(in []byte) error {
	rows, bad := 0, 0
	for _, line := range bytes.Split(in, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if len(t) == 0 {
			continue
		}
		rows++
		var r struct {
			Task string `json:"task"`
			Lane string `json:"lane"`
		}
		if json.Unmarshal(t, &r) != nil || r.Task == "" || r.Lane == "" {
			bad++
		}
	}
	if rows == 0 {
		return fmt.Errorf("no JSON rows: this does not look like an oracle")
	}
	if bad*2 > rows {
		return fmt.Errorf("%d of %d rows carry no task+lane identity: this does not look like an oracle (refusing to rewrite it)", bad, rows)
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d of %d line(s) are unreadable or identity-less; they are passed through VERBATIM, not stamped\n", bad, rows)
	}
	return nil
}

func migrateEffortFile(path string) (int, error) {
	in, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if err := looksLikeOracle(in); err != nil {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	out, changed, err := migrateEffort(in)
	if err != nil {
		return 0, err
	}
	if changed == 0 {
		return 0, nil // already migrated: touch nothing, not even the mtime
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("rename %s: %w", filepath.Base(tmp), err)
	}
	return changed, nil
}
