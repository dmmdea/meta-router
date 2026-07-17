package codexlane

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
)

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// binaryName is the codex executable Run spawns. It is a package-level var
// solely so timeout/fault tests can inject a portable sleeper (e.g. the
// test binary re-exec) in place of a real codex install (A2R-#8). Production
// always uses "codex".
var binaryName = "codex"

// versionAtLeast extracts the first x.y.z from raw and compares numerically.
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

// VersionGate runs `codex --version` once per process and reports whether the
// CLI is ≥ 0.142.5 — the trace-log prompt-leak fix (privacy gate; a leaked
// prompt is unrecoverable). `--force` (RunReq.SkipVersionGate) overrides,
// WARNed at the caller.
func VersionGate() (version string, ok bool) {
	versionOnce.Do(func() {
		out, err := exec.Command("codex", "--version").Output()
		if err != nil {
			cachedVersion = "unknown (" + err.Error() + ")"
			return
		}
		cachedVersion = strings.TrimSpace(string(out))
		cachedOK = versionAtLeast(cachedVersion, 0, 142, 5)
	})
	return cachedVersion, cachedOK
}

// VersionGateError is the PRODUCTION config_error message Run returns when the
// codex CLI is below the 0.142.5 privacy floor (A2R-#12). Exported and used by
// Run itself so the test asserts against the real message, not a hand-copied
// literal that can silently drift. It names both remedies: the upgrade command
// and the --force override.
func VersionGateError(version string) error {
	return fmt.Errorf("codex CLI %q is <0.142.5, which leaks prompts to trace logs — upgrade (npm i -g @openai/codex@latest) or rerun with --force", version)
}

// Run executes one codex exec --json turn. Same failure discipline as
// claudelane.Run: every path returns a CLASSIFIED Outcome; the error return
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
	// stdin-closed spawn: with Stdin nil, os/exec attaches the null device
	// (immediate EOF). codex otherwise BLOCKS FOREVER waiting on interactive
	// stdin when spawned headless — proven live 2026-07-06.
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "CODEX_HOME="+req.Home)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// F15 interim tree-kill (same as claudelane). Carry-forward note (also in
	// the evidence doc): revisit when `codex exec resume <id>`-based cancel/
	// resume is adopted — process-per-turn makes kill-and-rerun acceptable.
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
