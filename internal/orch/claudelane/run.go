package claudelane

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// Run executes one claude -p call. Failure discipline: every path returns a
// CLASSIFIED Outcome — a missing binary or dead spawn is "spawn_error", a
// nonzero exit that still produced JSON is parsed normally (the CLI reports
// error_during_execution results on stdout), garbage is "parse_error". The
// error return is reserved for arg-building (config) failures only, so
// callers always have an Outcome to dispatch-log.
func Run(ctx context.Context, req RunReq) (Outcome, []byte, error) {
	args, err := BuildArgs(req)
	if err != nil {
		return Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	if req.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	if req.CWD != "" {
		cmd.Dir = req.CWD
	}
	if len(req.Env) > 0 { // glm lane env pinning; empty = claude behavior, unchanged
		cmd.Env = append(os.Environ(), req.Env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Timeout discipline: with non-*os.File writers, Wait blocks on pipe-copy
	// goroutines that survive as long as any child holds the pipe. WaitDelay
	// forces Wait to return after cancel; on Windows the default Kill only
	// hits the direct child (claude.cmd), so Cancel kills the process TREE.
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
