// Package compact is W5's LOSSLESS JSON compaction (DG-3, operator-approved:
// "quality is the lever and the priority here. Compression is a
// savings/performance method that ranks second to quality"). Everything in
// this package is provably round-trippable — Decompact(Compact(x)) is
// semantically equal JSON — and anything LOSSY is deliberately absent: it
// belongs behind a fidelity gate + gold-set non-inferiority, per the decision
// gate, and the full lossy engine was delegated to the offload-harness
// session, not here.
//
// Two transforms, both meaning-preserving:
//  1. Minification — insignificant whitespace only (encoding/json.Compact).
//  2. Columnarization — a homogeneous array of ≥3 objects sharing one key
//     set becomes {"@columnar":{"cols":[...],"rows":[[...],...]}}, the
//     Headroom-style rotation that stops repeating every key on every row.
//     Self-describing (the marker names the transform) and exactly reversed
//     by Decompact.
//
// Safety rule: a document may legitimately contain "@columnar" keys of its
// own, and a lossless tier never guesses at decode. So the marker is ESCAPED
// bijectively before rotation — a pre-existing key of n '@'s + "columnar"
// gains one '@' on encode and loses it on decode — which makes every bare
// "@columnar" in Compact's output provably OURS. (An encode-side skip was
// not enough: Decompact of the passed-through document would have unrotated
// USER data — caught by the round-trip test, not by review.)
//
// The compaction is applied at EMBED time (a dep artifact being fenced into
// a downstream prompt); the stored artifact is never rewritten, so the
// original bytes remain recoverable regardless.
//
// WHAT THE CONTRACT PRESERVES, exactly (review 2026-08-12): values and
// structure under JSON semantics, INCLUDING number literals verbatim (the
// decode uses json.Number precisely because the float64 path silently
// rewrote int64s above 2^53 — reproduced live before this note existed).
// Duplicate-key documents never rotate (minify-only, both duplicates
// survive byte-wise — meaning under duplicates is parser-dependent, the
// same reason hostcfg refuses them). NOT preserved in re-encoded forms:
// byte identity, key order, insignificant whitespace, and escape-vs-literal
// spelling. Byte identity IS preserved on the minify-only paths.
package compact

import (
	"bytes"
	"encoding/json"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

const marker = "@columnar"

// escRe matches a key that is n≥1 '@'s followed by "columnar" — the escape
// family. Encode adds one '@'; decode removes one from n≥2 (a single-@ key in
// DECODE position is ours and unrotates instead).
var escRe = regexp.MustCompile(`^@+columnar$`)

// minRows is the columnarization floor: below it the header outweighs the
// savings and the rotation is pure churn.
const minRows = 3

// Compact losslessly compacts a JSON document. Non-JSON input returns
// ("", false) — prose is never touched. The result is the SMALLER of the
// minified and columnarized forms; applied reports whether out differs from
// raw (even pure minification counts — the caller decides whether to embed).
func Compact(raw string) (out string, applied bool) {
	if !json.Valid([]byte(raw)) {
		return "", false
	}
	var min bytes.Buffer
	if err := json.Compact(&min, []byte(raw)); err != nil {
		return "", false
	}
	best := min.String()
	// Duplicate keys make a document's meaning parser-dependent (the exact
	// reason hostcfg refuses them): a decode-re-encode would silently DELETE
	// all but the last duplicate — and deletion shrinks the doc, so the
	// corrupted form would tend to WIN the length contest. Minify-only for
	// such documents: byte-preserving, both duplicates survive.
	if hasDupKeys([]byte(raw)) {
		return best, best != raw
	}
	if v, err := decodeNumeric(raw); err == nil {
		esc, family := escapeKeys(v, 1)
		if family {
			// The document uses the marker family itself. Every candidate
			// output MUST be escaped — emitting the raw minified form here
			// would hand Decompact a bare user marker to destroy (the exact
			// loss the round-trip test caught). One byte per family key is
			// the price of an unambiguous decode.
			eb, err := marshalPlain(esc)
			if err != nil {
				return raw, false // unreachable for valid JSON — decline rather than emit a half-guessed form
			}
			best = string(eb)
		}
		if col, err := marshalPlain(columnarize(esc)); err == nil && len(col) < len(best) {
			best = string(col)
		}
	}
	return best, best != raw
}

// decodeNumeric decodes JSON preserving NUMBER LITERALS exactly (UseNumber):
// the plain any-decode goes through float64 and silently rewrites integers
// above 2^53 — receipts carry int64 token counts, epoch-nanos and IDs live in
// that range, and a rewritten ID is the precise silent quality-corruption the
// lossless contract forbids (review 2026-08-12, reproduced on the rotation's
// own target archetype with the round-trip judge blind to it).
func decodeNumeric(raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// marshalPlain encodes WITHOUT HTML escaping: the default Marshal rewrites
// < > & as \u-escapes, which visually mangles code-bearing strings for the
// MODEL that reads the embedded form (no model ever calls Decompact) and
// inflates the byte count the length contest optimizes. json.Number values
// print as their original literals.
func marshalPlain(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// hasDupKeys walks the token stream checking every object for a repeated key.
func hasDupKeys(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		obj  bool
		keys map[string]bool
		key  bool // next string token is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return false // io.EOF on well-formed input (json.Valid ran first)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{obj: true, keys: map[string]bool{}, key: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return false
				}
				if top := stack[len(stack)-1]; top.obj {
					top.key = true
				}
			}
		case string:
			if len(stack) > 0 {
				if top := stack[len(stack)-1]; top.obj && top.key {
					if top.keys[t] {
						return true
					}
					top.keys[t] = true
					top.key = false
					continue
				}
			}
			if len(stack) > 0 && stack[len(stack)-1].obj {
				stack[len(stack)-1].key = true // this string was a VALUE; next is a key
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1].obj {
				stack[len(stack)-1].key = true
			}
		}
	}
}

// Decompact reverses Compact's columnarization, returning minified JSON that
// is semantically equal to the original document. ok=false when in is not
// valid JSON.
func Decompact(in string) (out string, ok bool) {
	v, err := decodeNumeric(in)
	if err != nil {
		return "", false
	}
	dec, _ := escapeKeys(decolumnarize(v), -1)
	b, err := marshalPlain(dec)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// EmbedSafe is the form safe to show a consumer that will NEVER run
// Decompact — an LLM reading the fenced context. It differs from Compact in
// exactly one case: a document using the marker family itself is NOT
// compacted at all (ok=false, embed the original), because the bijective
// escape only holds for round-tripping callers — a model shown "@@columnar"
// for a key the source spelled "@columnar" is reading data the source never
// contained, which is loss in the one direction that ships (review
// 2026-08-12).
func EmbedSafe(raw string) (out string, ok bool) {
	if v, err := decodeNumeric(raw); err == nil {
		if _, family := escapeKeys(v, 1); family {
			return "", false
		}
	}
	out, applied := Compact(raw)
	return out, applied
}

// escapeKeys shifts every escape-family key by delta '@'s (encode +1, decode
// −1; decode never touches the single-@ marker itself — that one unrotates).
// touched reports whether any family key was seen, in either direction.
func escapeKeys(v any, delta int) (out any, touched bool) {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			nk := k
			if escRe.MatchString(k) {
				touched = true
				ats := strings.IndexByte(k, 'c') // count of leading '@'s
				if delta > 0 {
					nk = "@" + k
				} else if ats >= 2 {
					nk = k[1:]
				}
			}
			ne, sub := escapeKeys(e, delta)
			touched = touched || sub
			m[nk] = ne
		}
		return m, touched
	case []any:
		a := make([]any, len(t))
		for i, e := range t {
			var sub bool
			a[i], sub = escapeKeys(e, delta)
			touched = touched || sub
		}
		return a, touched
	default:
		return v, false
	}
}

// Equal reports deep semantic equality of two JSON documents — the round-trip
// contract's judge (byte equality is deliberately NOT the contract: key order
// and whitespace are canonicalized by encoding/json). Numbers are compared as
// exact rationals from their LITERALS, never through float64 — a judge that
// canonicalized through the same float64 the code under test used was blind
// to the one loss class the code actually had (review 2026-08-12).
func Equal(a, b string) bool {
	va, erra := decodeNumeric(a)
	vb, errb := decodeNumeric(b)
	if erra != nil || errb != nil {
		return false
	}
	return reflect.DeepEqual(normNumbers(va), normNumbers(vb))
}

// normNumbers canonicalizes every json.Number to an exact rational string so
// 1, 1.0 and 1e0 compare equal while adjacent int64s never collide.
func normNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if r, ok := new(big.Rat).SetString(string(t)); ok {
			return r.RatString()
		}
		return string(t) // exotic literal — compare verbatim, never coerce
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normNumbers(e)
		}
		return out
	default:
		return v
	}
}

// columnarize walks a decoded JSON value, rotating qualifying arrays.
func columnarize(v any) any {
	switch t := v.(type) {
	case []any:
		if cols, rows, ok := rotate(t); ok {
			return map[string]any{marker: map[string]any{"cols": cols, "rows": rows}}
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = columnarize(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = columnarize(e)
		}
		return out
	default:
		return v
	}
}

// rotate turns an array of ≥minRows objects sharing an identical key set into
// (cols, rows). Values are columnarized recursively.
func rotate(arr []any) (cols []string, rows [][]any, ok bool) {
	if len(arr) < minRows {
		return nil, nil, false
	}
	first, isObj := arr[0].(map[string]any)
	if !isObj {
		return nil, nil, false
	}
	cols = make([]string, 0, len(first))
	for k := range first {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	for _, e := range arr {
		obj, isObj := e.(map[string]any)
		if !isObj || len(obj) != len(cols) {
			return nil, nil, false
		}
		row := make([]any, len(cols))
		for i, k := range cols {
			val, present := obj[k]
			if !present {
				return nil, nil, false
			}
			row[i] = columnarize(val)
		}
		rows = append(rows, row)
	}
	return cols, rows, true
}

// decolumnarize reverses the rotation wherever the marker appears.
func decolumnarize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if inner, isMarker := t[marker].(map[string]any); isMarker {
				if arr, ok := unrotate(inner); ok {
					return arr
				}
			}
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = decolumnarize(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = decolumnarize(e)
		}
		return out
	default:
		return v
	}
}

func unrotate(inner map[string]any) ([]any, bool) {
	rawCols, cok := inner["cols"].([]any)
	rawRows, rok := inner["rows"].([]any)
	if !cok || !rok || len(inner) != 2 {
		return nil, false
	}
	cols := make([]string, len(rawCols))
	for i, c := range rawCols {
		s, isStr := c.(string)
		if !isStr {
			return nil, false
		}
		cols[i] = s
	}
	out := make([]any, len(rawRows))
	for i, rr := range rawRows {
		row, isRow := rr.([]any)
		if !isRow || len(row) != len(cols) {
			return nil, false
		}
		obj := make(map[string]any, len(cols))
		for j, k := range cols {
			obj[k] = decolumnarize(row[j])
		}
		out[i] = obj
	}
	return out, true
}
