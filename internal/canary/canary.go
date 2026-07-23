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
