package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A stand-in for `claude` / `codex` on PATH. It is this same test binary,
// copied under the host's name and re-entered from TestMain, so the installer's
// real spawn path — LookPath, the scrubbed environment, the pinned HOME, the
// exit code — is exercised for real while the writes land in a scratch home.
//
// It implements only what the installer calls: `mcp add` and `mcp remove`.
func runHostStub(argv []string) int {
	who := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if logPath := os.Getenv("MR_TEST_STUB_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, who+" "+strings.Join(argv, " "))
			f.Close()
		}
	}
	if os.Getenv("MR_TEST_STUB_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "stub: refusing to register")
		return 1
	}
	if len(argv) < 3 || argv[0] != "mcp" {
		fmt.Fprintln(os.Stderr, "stub: unexpected argv", argv)
		return 2
	}
	home := os.Getenv("USERPROFILE")
	if home == "" {
		home = os.Getenv("HOME")
	}
	if os.Getenv("MR_TEST_STUB_REWRITE") == "1" {
		rewriteSettingsLikeTheHost(filepath.Join(home, ".claude", "settings.json"))
	}
	switch who {
	case "claude":
		return stubClaudeMCP(filepath.Join(home, ".claude.json"), argv[1])
	case "codex":
		dir := os.Getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		return stubCodexMCP(filepath.Join(dir, "config.toml"), argv[1])
	}
	fmt.Fprintln(os.Stderr, "stub: unknown host", who)
	return 2
}

// rewriteSettingsLikeTheHost reproduces a behaviour of the REAL Claude Code
// cli, observed 2026-08-11: ANY invocation — including a read-only
// `claude mcp list` — rewrites ~/.claude/settings.json, normalising the `model`
// value and re-indenting the document. An installer that hashed the bytes it
// wrote would therefore record a hash that is stale before it returns, and
// every later uninstall would refuse the byte-identical path.
func rewriteSettingsLikeTheHost(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return
	}
	if _, ok := doc["model"]; ok {
		doc["model"] = json.RawMessage(`"opus[1m]"`)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, out, 0o644)
}

func stubClaudeMCP(path, verb string) int {
	doc := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return 3
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return 3
		}
	}
	switch verb {
	case "add":
		servers[mcpServerName] = json.RawMessage(`{"command":"mr-orchestrate","args":["mcp"]}`)
	case "remove":
		delete(servers, mcpServerName)
	default:
		return 2
	}
	raw, err := json.Marshal(servers)
	if err != nil {
		return 3
	}
	doc["mcpServers"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 3
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 3
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return 3
	}
	return 0
}

func stubCodexMCP(path, verb string) int {
	const header = "[mcp_servers." + mcpServerName + "]"
	body, _ := os.ReadFile(path)
	lines := strings.Split(string(body), "\n")
	var kept []string
	skipping := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			skipping = t == header
		}
		if !skipping {
			kept = append(kept, ln)
		}
	}
	out := strings.Join(kept, "\n")
	if verb == "add" {
		if !strings.HasSuffix(out, "\n") && out != "" {
			out += "\n"
		}
		out += header + "\ncommand = \"mr-orchestrate\"\nargs = [\"mcp\"]\n"
	} else if verb != "remove" {
		return 2
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 3
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return 3
	}
	return 0
}

var (
	hostStubOnce sync.Once
	hostStubDir  string
	hostStubErr  error
)

// hostStubOnPATH copies this test binary to `claude` and `codex` in a shared
// temp dir and returns it, ready to be PREPENDED to PATH.
func hostStubOnPATH(t *testing.T) string {
	t.Helper()
	hostStubOnce.Do(func() {
		self, err := os.Executable()
		if err != nil {
			hostStubErr = err
			return
		}
		dir, err := os.MkdirTemp("", "mr-hoststub-")
		if err != nil {
			hostStubErr = err
			return
		}
		for _, name := range []string{"claude", "codex"} {
			if err := copyExe(self, filepath.Join(dir, exeName(name))); err != nil {
				hostStubErr = err
				return
			}
		}
		hostStubDir = dir
	})
	if hostStubErr != nil {
		t.Fatalf("host stub: %v", hostStubErr)
	}
	return hostStubDir
}

func cleanupHostStub() {
	if hostStubDir != "" {
		os.RemoveAll(hostStubDir)
	}
}

func copyExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
