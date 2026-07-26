// Package canary enforces ROUTER_BIBLE.md invariants as tests over the actual
// source tree. A canary is deliberately brittle: its job is to make a silent
// concept change loud. Changing what a canary pins requires the CONCEPT-CHANGE
// protocol (see ROUTER_BIBLE.md), never a quiet test edit.
package canary

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RepoRoot walks up from the working directory to the first dir with go.mod.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// GoSourceFiles lists .go files under root, skipping testdata and .git always,
// and _test.go files unless includeTests.
func GoSourceFiles(root string, includeTests bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if !includeTests && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// StripGoComments removes // and /* */ comments from Go source, leaving string
// literals intact. It also normalizes CRLF to LF.
//
// Why a canary needs this: a source-scanning canary that asks
// strings.Contains(src, "childenv.Scrub") is satisfied by a COMMENT saying
// "childenv.Scrub is called below" — so deleting the actual call still passes.
// That is not a hypothetical: the B14 gate check passed with its only
// egress.Plan CALL removed, because the explanatory comment above it survived
// (mutation-tested 2026-07-25). Every canary that greps for a call must scan
// code, not prose.
//
// Deliberately conservative about the failure direction: string literals are
// preserved so a URL like "http://x" is never mistaken for a comment. Anything
// this misreads costs a false FAILURE, which is loud, rather than a false pass.
func StripGoComments(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(src))
	const (
		code = iota
		lineComment
		blockComment
		str    // "..."
		rawStr // `...`
		chr    // '...'
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				i++
			default:
				switch c {
				case '"':
					state = str
				case '`':
					state = rawStr
				case '\'':
					state = chr
				}
				b.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				b.WriteByte(c) // keep line structure for body-boundary regexes
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			} else if c == '\n' {
				b.WriteByte(c)
			}
		case str, chr:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
				continue
			}
			if (state == str && c == '"') || (state == chr && c == '\'') {
				state = code
			}
		case rawStr:
			b.WriteByte(c)
			if c == '`' {
				state = code
			}
		}
	}
	return b.String()
}

// B1Forbidden is the single source of truth for the B1 API-key-auth pattern:
// env reads (Getenv/LookupEnv) of *_API_KEY / *APIKEY names, or a quoted
// x-api-key header literal. The header token is concatenated so this
// definition never flags itself in the source scan.
var B1Forbidden = regexp.MustCompile(`(?i)(Getenv|LookupEnv)\("[^"]*API_?KEY[^"]*"\)|` + `"x-api` + `-key"`)

// ScanForbidden returns "path:line: text" for every line matching re.
func ScanForbidden(files []string, re *regexp.Regexp) ([]string, error) {
	var hits []string
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		line := 0
		for sc.Scan() {
			line++
			if re.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", f, line, strings.TrimSpace(sc.Text())))
			}
		}
		serr := sc.Err()
		fh.Close()
		if serr != nil {
			return nil, serr
		}
	}
	return hits, nil
}
