package copilotlane

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/childenv"
)

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// binaryName is the copilot executable Run spawns; a package-level var solely
// so timeout/fault tests can inject a portable stand-in (the codexlane A2R-#8
// pattern). Production always uses "copilot".
var binaryName = "copilot"

// ghBinaryName is the gh executable MintToken spawns; var for the same
// test-injection reason.
var ghBinaryName = "gh"

func versionAtLeast(raw string, maj, min, patch int) bool {
	m := versionRe.FindString(raw)
	if m == "" {
		return false
	}
	var a, b, c int
	if _, err := fmt.Sscanf(m, "%d.%d.%d", &a, &b, &c); err != nil {
		return false
	}
	if a != maj {
		return a > maj
	}
	if b != min {
		return b > min
	}
	return c >= patch
}

var (
	versionOnce   sync.Once
	cachedVersion string
	cachedOK      bool
)

// VersionGate runs `copilot --version` once per process and reports whether
// the CLI is ≥ 1.0.82 — the floor this lane's flag surface and JSONL parse
// contract were live-verified against (2026-09-01). A COMPAT gate, not a
// privacy gate: older CLIs may lack --deny-tool/--output-format json, and a
// dispatch whose isolation flags are silently unknown must not run.
func VersionGate() (version string, ok bool) {
	versionOnce.Do(func() {
		vc := exec.Command(binaryName, "--version")
		vc.Env = childenv.Scrub(os.Environ())
		out, err := vc.Output()
		if err != nil {
			cachedVersion = "unknown (" + err.Error() + ")"
			return
		}
		cachedVersion = strings.TrimSpace(string(out))
		cachedOK = versionAtLeast(cachedVersion, 1, 0, 82)
	})
	return cachedVersion, cachedOK
}

// VersionGateError names both remedies, mirroring codexlane's exported-error
// discipline (the test asserts the real message, not a copy).
func VersionGateError(version string) error {
	return fmt.Errorf("copilot CLI %q is <1.0.82, below the flag surface this lane was verified against — upgrade (npm i -g @github/copilot@latest) or rerun with --force", version)
}

// MintToken resolves the operator-configured GitHub account to an OAuth token
// via `gh auth token --user <user>`. Minted per dispatch, never persisted,
// never read from ambient env: the gh keyring is the single credential
// source, and pinning the user makes the mutable "active account" irrelevant
// (the account-flip hazard is documented ecosystem-wide).
func MintToken(user string) (string, error) {
	if user == "" {
		return "", fmt.Errorf("copilot_token_user is empty: set it in the orchestrator config to the GitHub account that owns the Copilot subscription")
	}
	c := exec.Command(ghBinaryName, "auth", "token", "--user", user)
	c.Env = childenv.Scrub(os.Environ())
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token --user %s failed (is that account logged in to gh?): %w", user, err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("gh auth token --user %s returned empty output", user)
	}
	return tok, nil
}

// Run executes one non-interactive copilot dispatch. Same failure discipline
// as codexlane.Run: every path returns a CLASSIFIED Outcome; the error return
// is reserved for config failures (bad args, version gate) so callers always
// have an Outcome to dispatch-log.
func Run(ctx context.Context, req RunReq) (Outcome, []byte, error) {
	args, err := BuildArgs(req)
	if err != nil {
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	if !req.SkipVersionGate {
		if v, ok := VersionGate(); !ok {
			gateErr := VersionGateError(v)
			return Outcome{Class: "config_error", Result: gateErr.Error()}, nil, gateErr
		}
	}
	if req.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, binaryName, args...)
	if req.CWD != "" {
		cmd.Dir = req.CWD
	}
	// stdin-closed spawn: the codex lane proved a CLI lane can block forever
	// on interactive stdin under the orchestrator; null-device stdin is the
	// standing discipline for every lane spawn.
	cmd.Stdin = nil
	// R10 boundary: scrub ambient credential/routing vars, then append this
	// lane's TWO deliberate pins — the isolated home (keeps the operator's
	// desktop ~/.copilot config out of orchestrated dispatches) and the
	// per-dispatch token (COPILOT_GITHUB_TOKEN is first in the CLI's
	// documented precedence, so a survivor GH_TOKEN could never outrank it
	// even if scrubbing missed one).
	cmd.Env = append(childenv.Scrub(os.Environ()),
		"COPILOT_HOME="+req.Home,
		"COPILOT_GITHUB_TOKEN="+req.Token,
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 10 * time.Second
	if runtime.GOOS == "windows" {
		cmd.Cancel = func() error {
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
	}
	if runErr := cmd.Run(); runErr != nil {
		if out.Len() > 0 {
			return Parse(out.Bytes()), out.Bytes(), nil
		}
		msg := runErr.Error()
		if errb.Len() > 0 {
			msg += ": " + errb.String()
		}
		return Outcome{Class: "spawn_error", Result: msg}, nil, nil
	}
	return Parse(out.Bytes()), out.Bytes(), nil
}
