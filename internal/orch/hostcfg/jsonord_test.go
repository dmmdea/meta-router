package hostcfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole reason this package exists instead of map[string]any: an installer
// that reorders and re-indents the operator's config on every run produces a
// diff nobody can review, in a file whose corruption disables every hook.
const sample = `{
  "env": {
    "FOO": "bar",
    "N": 1.50
  },
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
  "_note": "hand-written, order matters"
}
`

func TestUneditedRoundTripIsByteIdentical(t *testing.T) {
	o, err := parseObject([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	got := string(o.Encode(detectStyle([]byte(sample)), 0)) + "\n"
	if got != sample {
		t.Fatalf("round trip changed the document.\n--- want ---\n%s\n--- got ---\n%s", sample, got)
	}
}

// Every one of these is a shape a real settings.json takes on this fleet, and
// each one broke the byte-preservation promise in a different way.
func TestFormattingIsPreservedAcrossRealWorldShapes(t *testing.T) {
	t.Run("CRLF file does not come back with mixed endings", func(t *testing.T) {
		// A git-tracked settings.json on a Windows fleet: mixed endings are a
		// whole-file diff on the next commit.
		src := "{\r\n  \"model\": \"opus\",\r\n  \"keep\": {\r\n    \"a\": 1\r\n  }\r\n}\r\n"
		d := docFrom(t, src)
		if err := d.MemberSet([]string{"statusLine"}, json.RawMessage(`{"type":"command","command":"tee"}`)); err != nil {
			t.Fatal(err)
		}
		out := string(d.Bytes())
		if bare := strings.Count(out, "\n") - strings.Count(out, "\r\n"); bare != 0 {
			t.Fatalf("%d bare LFs in a CRLF document:\n%q", bare, out)
		}
	})

	t.Run("a blank line before the first member does not flatten the file", func(t *testing.T) {
		// detectIndent used to read the line after the FIRST break, so a blank
		// line meant "no indentation" and the whole document collapsed onto one
		// line while nested values kept their pretty-printed bytes.
		src := "{\n\n  \"model\": \"opus\",\n  \"deep\": {\n    \"a\": 1\n  }\n}\n"
		d := docFrom(t, src)
		if err := d.MemberSet([]string{"x"}, json.RawMessage(`1`)); err != nil {
			t.Fatal(err)
		}
		out := string(d.Bytes())
		if !strings.Contains(out, "\n  \"model\": \"opus\"") {
			t.Fatalf("document was flattened:\n%s", out)
		}
	})

	t.Run("key spelling survives exactly as written", func(t *testing.T) {
		// Re-marshalling the DECODED key rewrote members nobody asked to touch:
		// encoding/json HTML-escapes `<`, `>` and `&`, so "a<b>c&d" came back as
		// "a<b>c&d". Preservation runs the other way too — a key
		// the operator wrote as é stays é rather than being helpfully
		// unescaped.
		src := "{\n  \"caf\\u00e9\": 1,\n  \"a<b>c&d\": 2\n}\n"
		d := docFrom(t, src)
		if err := d.MemberSet([]string{"added"}, json.RawMessage(`1`)); err != nil {
			t.Fatal(err)
		}
		out := string(d.Bytes())
		// Taken from src itself, so the assertion cannot drift from the input:
		// both keys must appear byte-for-byte as the source spelled them.
		for _, want := range []string{"\"caf\\u00e9\"", "\"a<b>c&d\""} {
			if !strings.Contains(src, want) {
				t.Fatalf("bad fixture: %s is not in the source", want)
			}
			if !strings.Contains(out, want) {
				t.Fatalf("key %s was re-spelled:\n%s", want, out)
			}
		}
	})

	t.Run("a UTF-8 BOM is kept, not choked on", func(t *testing.T) {
		// PowerShell 5.1's Set-Content writes one by default, so this is a live
		// fleet state. Without handling, the decoder failed with an opaque
		// `invalid character 'ï'` and the install aborted.
		p := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(p, append([]byte{0xEF, 0xBB, 0xBF}, []byte("{\n  \"model\": \"opus\"\n}\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		d, _, err := LoadDoc(p)
		if err != nil {
			t.Fatalf("a BOM must not abort the load: %v", err)
		}
		if err := d.MemberSet([]string{"x"}, json.RawMessage(`1`)); err != nil {
			t.Fatal(err)
		}
		if got := d.Bytes(); !bytes.HasPrefix(got, []byte{0xEF, 0xBB, 0xBF}) {
			t.Fatalf("the BOM was dropped: %q", got)
		}
	})
}

// Two members with one name: parsers disagree about which wins, so a document
// whose meaning is parser-dependent is one this package must not rewrite.
// Last-wins silently deleted a member of the operator's file.
func TestDuplicateKeysAreRefused(t *testing.T) {
	_, err := parseObject([]byte(`{"model":"opus","model":"sonnet"}`))
	if err == nil {
		t.Fatal("a duplicate key must be refused, not silently collapsed")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("the error should name the problem: %v", err)
	}
}

func TestMemberSetRefusesNonJSON(t *testing.T) {
	// reindent falls back to its input when json.Indent fails, so an invalid
	// value used to be stored verbatim, rendered into the document, and saved
	// with a nil error — producing exactly the invalid settings.json this
	// package exists to prevent.
	d := docFrom(t, sample)
	for name, bad := range map[string]json.RawMessage{
		"nil":       nil,
		"empty":     json.RawMessage(""),
		"truncated": json.RawMessage(`{"a":`),
	} {
		if err := d.MemberSet([]string{"statusLine"}, bad); err == nil {
			t.Fatalf("%s value was accepted", name)
		}
		if err := d.ArrayAppend([]string{"hooks", "SessionStart"}, bad); err == nil {
			t.Fatalf("%s element was accepted", name)
		}
	}
	if got := string(d.Bytes()); got != sample {
		t.Fatalf("a refused write must leave the document untouched:\n%s", got)
	}
}

func TestEditLeavesEveryOtherMemberUntouched(t *testing.T) {
	d := docFrom(t, sample)
	if err := d.MemberSet([]string{"statusLine"}, json.RawMessage(`{"type":"command","command":"tee"}`)); err != nil {
		t.Fatal(err)
	}
	out := string(d.Bytes())
	// Key order preserved, and the odd scalar spelling survives: a
	// decode-to-any round trip would have rewritten 1.50 as 1.5.
	for _, frag := range []string{`"N": 1.50`, `"_note": "hand-written, order matters"`, `"theirs.exe"`} {
		if !strings.Contains(out, frag) {
			t.Fatalf("edit rewrote an untouched member: %q missing from\n%s", frag, out)
		}
	}
	if strings.Index(out, `"env"`) > strings.Index(out, `"model"`) {
		t.Fatalf("member order was not preserved:\n%s", out)
	}
	if !strings.Contains(out, `"statusLine": {`) {
		t.Fatalf("new member was not indented into the document:\n%s", out)
	}
	if _, err := parseObject(d.Bytes()); err != nil {
		t.Fatalf("edited document is not valid JSON: %v", err)
	}
}

func TestArrayAppendKeepsNeighbours(t *testing.T) {
	d := docFrom(t, sample)
	path := []string{"hooks", "SessionStart"}
	if err := d.ArrayAppend(path, json.RawMessage(`{"hooks":[{"type":"command","command":"ours.exe"}]}`)); err != nil {
		t.Fatal(err)
	}
	n, present, err := d.ArrayLen(path)
	if err != nil || !present || n != 2 {
		t.Fatalf("append: len=%d present=%v err=%v", n, present, err)
	}
	if !strings.Contains(string(d.Bytes()), "theirs.exe") {
		t.Fatalf("append dropped the existing entry:\n%s", d.Bytes())
	}
	// A path whose array does not exist yet is created, not an error: a host
	// with no SessionStart hooks at all is a normal fresh machine.
	if err := d.ArrayAppend([]string{"hooks", "UserPromptSubmit"}, json.RawMessage(`{"hooks":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := d.ArrayLen([]string{"hooks", "UserPromptSubmit"}); !present {
		t.Fatal("append did not create a missing array")
	}
	if _, err := parseObject(d.Bytes()); err != nil {
		t.Fatalf("document invalid after appends: %v\n%s", err, d.Bytes())
	}
}

func TestParseRejectsTrailingContent(t *testing.T) {
	// A config file with a second document in it is corrupt. Editing only the
	// first half and writing that back is how a truncated write becomes
	// permanent — the exact failure that once left settings.json invalid and
	// every rule silently inert.
	if _, err := parseObject([]byte(`{"a":1}{"b":2}`)); err == nil {
		t.Fatal("trailing content must be rejected, not silently dropped")
	}
	if _, err := parseObject([]byte(`{"a":1}` + "\n\n  ")); err != nil {
		t.Fatalf("trailing whitespace is not trailing content: %v", err)
	}
}

func TestDetectStyle(t *testing.T) {
	for _, tc := range []struct{ in, indent, nl string }{
		{"{\n  \"a\": 1\n}", "  ", "\n"},
		{"{\n    \"a\": 1\n}", "    ", "\n"},
		{"{\n\t\"a\": 1\n}", "\t", "\n"},
		{"{\r\n  \"a\": 1\r\n}", "  ", "\r\n"},
		{"{\n\n  \"a\": 1\n}", "  ", "\n"}, // blank line first
		{`{"a":1}`, "", "\n"},
	} {
		got := detectStyle([]byte(tc.in))
		if got.indent != tc.indent || got.nl != tc.nl {
			t.Fatalf("detectStyle(%q) = {%q,%q}, want {%q,%q}", tc.in, got.indent, got.nl, tc.indent, tc.nl)
		}
	}
}

func TestMinifiedStaysMinified(t *testing.T) {
	d := docFrom(t, `{"a":1,"b":2}`)
	if err := d.MemberSet([]string{"c"}, json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := string(d.Bytes()); strings.Contains(got, "\n") {
		t.Fatalf("an installer asked to add one key must not pretty-print a minified config: %s", got)
	}
}

func TestSameValueComparesMeaningNotSpelling(t *testing.T) {
	same, err := SameValue(
		json.RawMessage(`{"type":"command","command":"x"}`),
		json.RawMessage("{\n  \"command\": \"x\",\n  \"type\": \"command\"\n}"))
	if err != nil || !same {
		t.Fatalf("reordered keys and whitespace are not a change: same=%v err=%v", same, err)
	}
	same, err = SameValue(json.RawMessage(`{"command":"x"}`), json.RawMessage(`{"command":"y"}`))
	if err != nil || same {
		t.Fatalf("a different command IS a change: same=%v err=%v", same, err)
	}
	if _, err := SameValue(json.RawMessage(`{`), json.RawMessage(`{}`)); err == nil {
		t.Fatal("an unparseable side must be an error, never a silent 'not equal'")
	}
}

// A file that cannot be READ is not an absent file. Collapsing the two is how
// an installer overwrites a config it merely failed to open, and how an
// uninstaller reports a clean removal it never performed.
func TestUnreadableIsNeverAbsent(t *testing.T) {
	dir := t.TempDir()
	if _, p, err := ReadFile(filepath.Join(dir, "nope.json")); p != AbsentFile || err != nil {
		t.Fatalf("missing file: presence=%v err=%v", p, err)
	}
	// Reading a directory as a file fails on every platform this ships to, and
	// the failure is emphatically not os.IsNotExist.
	sub := filepath.Join(dir, "adir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, p, err := ReadFile(sub)
	if p != UnknownFile || err == nil {
		t.Fatalf("an unreadable path must be UnknownFile with the real error, got presence=%v err=%v", p, err)
	}
	if _, _, err := LoadDoc(sub); err == nil {
		t.Fatal("LoadDoc must refuse an unreadable path rather than return an empty document")
	}
}

func TestLoadDocOfAbsentFileIsEmptyNotAnError(t *testing.T) {
	d, p, err := LoadDoc(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil || p != AbsentFile {
		t.Fatalf("presence=%v err=%v", p, err)
	}
	if got := string(d.Bytes()); got != "{}\n" {
		t.Fatalf("an absent config must load as an empty document, got %q", got)
	}
}

func TestWriteAtomicReplacesWholeFile(t *testing.T) {
	// The shrink case is the one that bit this fleet: a non-atomic write that
	// shortened settings.json left the old tail behind and produced invalid
	// JSON, which silently made every rule and hook inert.
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteAtomic(p, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "short" {
		t.Fatalf("shrinking write left a tail: %d bytes", len(b))
	}
}

func TestManifestSchemaMismatchRefuses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(p, []byte(`{"schema":99,"host":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadManifest(p); err == nil {
		t.Fatal("a manifest from an unknown schema must refuse, not be acted on")
	}
}

func docFrom(t *testing.T, body string) *Doc {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d, _, err := LoadDoc(p)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
