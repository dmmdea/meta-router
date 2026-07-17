// Package locallane is the BLACK-BOX adapter to the local-offload binaries.
// Slice-3 scope boundary (design brief §1): meta-router owns ONLY this thin
// adapter — it shells out to the local tools exactly as meta-router shells out
// to claude/codex, and maps their returns into the standard claudelane.Outcome.
// It NEVER drives their internals, endpoint, roster, or a UI (R1c boundary).
//
// S3R-1 — TWO DOORS, both CLI shell-outs, keyed on the resolved model/class:
//
//   - Cascade door → the offload-harness CLI (the offload_* Gemma cascade). For
//     the grunt/verify classes (mechanical-text, doc-summarize, verify-gate) and
//     the cascade models (gemma4-cascade, qwythos). The harness self-routes its
//     own model tier, so we pass NO --model. It returns a structured DEFER when it
//     can't do the task confidently — mapped to a `deferred` RELEGATION outcome
//     (never a false `ok`, never an error): a deferred local node lets the DAG
//     relegate/escalate to a cloud alternative — the whole point of the local
//     lane's honest-defer contract.
//   - Agent door → the local-agent CLI (the agentic loop) for the agentic class
//     (hard-case-reclaim) / model (qwythos-think). Read-only by default (no
//     --allow-write/-fetch/-shell).
//
// Fail-open (R8): a missing binary / cold endpoint / nonzero exit with no
// parseable output yields a CLASSIFIED spawn_error Outcome — never a Go error,
// never a panic — so a strategy node degrades to a relegation. Same WaitDelay +
// Windows taskkill /T /F tree-kill as codexlane.
//
// R10 keyless: local is the FREE lane — both doors pass NO keys/tokens; the
// adapter transmits nothing but the prompt and the read-only workspace root.
package locallane

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/claudelane"
)

// Door identifies which local surface a node routes to. Exported so the caller
// (mr-orchestrate) picks the binary from ONE source of truth for the door rule.
type Door int

const (
	DoorCascade Door = iota // offload-harness (offload_* Gemma cascade)
	DoorAgent               // local-agent (agentic loop)
)

// internal aliases so the existing tests read cleanly.
const (
	doorCascade = DoorCascade
	doorAgent   = DoorAgent
)

// cascadeVerbFor maps a resolved class to the offload-harness cascade verb.
// verify-gate is a yes/no gate → triage; the grunt classes → summarize (the
// default cascade verb). Verified read-only against the offload-harness CLI
// (main.go: verbs summarize|classify|extract|triage, `<verb> - --json` reading
// stdin) — verify the exact verb/flag at execution, like the agent-door note.
func cascadeVerbFor(class string) string {
	switch class {
	case "verify-gate":
		return "triage"
	default:
		// doc-summarize / mechanical-text / anything else cascade-bound → summarize.
		return "summarize"
	}
}

// ResolveDoor is the S3R-1 door-routing rule, keyed on the RESOLVED class/model.
// The agentic class (hard-case-reclaim) or the agentic model (qwythos-think)
// takes the agent door; everything else — the cascade classes/models AND the
// ambiguous default — takes the cascade door (cheaper/faster). Returns the door
// and, for the cascade door, the verb to invoke ("" for the agent door). This is
// the SINGLE source of truth for the door rule; the caller uses it to pick the
// binary.
func ResolveDoor(class, model string) (Door, string) {
	if class == "hard-case-reclaim" || model == "qwythos-think" {
		return DoorAgent, ""
	}
	return DoorCascade, cascadeVerbFor(class)
}

// resolveDoor is the internal alias kept for the existing tests.
func resolveDoor(class, model string) (Door, string) { return ResolveDoor(class, model) }

// ── Agent door ────────────────────────────────────────────────────────────

// agentResult mirrors local-agent's --json output (agent.Result, no json tags →
// Go default exported-field names). VERIFY AT EXECUTION if the binary changes shape.
type agentResult struct {
	Output     string          `json:"Output"`
	Steps      int             `json:"Steps"`
	StopReason string          `json:"StopReason"`
	Transcript json.RawMessage `json:"Transcript"`
}

// classFromStop maps local-agent StopReason → the standard outcome_class. The
// gate is "ok" only on a finished run WITH content; budget/error/garbage never ok.
func classFromStop(stop, output string) string {
	switch stop {
	case "done":
		if output == "" {
			return "empty_result" // finished but no answer — never ok (RS6 posture)
		}
		return "ok"
	case "budget":
		return "incomplete"
	case "error":
		return "api_error"
	default:
		return "parse_error"
	}
}

// Run shells out to the local-agent binary (agent door) as a black box and
// returns a classified Outcome + raw stdout. Fail-open: every failure is a
// classified Outcome, so the error return is always nil here.
func Run(ctx context.Context, bin, objective, root string, timeoutSec int) (claudelane.Outcome, []byte, error) {
	if root == "" {
		root = "."
	}
	// Read-only by default: no --allow-write/-fetch/-shell. --json for the machine
	// result. objective is a positional (local-agent's splitObjective handles order).
	args := []string{objective, "--root", root, "--json"}
	out, errb, runErr := runCmd(ctx, bin, args, timeoutSec)
	if runErr != nil {
		if len(out) > 0 {
			return parseAgent(out), out, nil // nonzero exit that still produced JSON
		}
		return spawnErr(runErr, errb), nil, nil
	}
	return parseAgent(out), out, nil
}

func parseAgent(raw []byte) claudelane.Outcome {
	var r agentResult
	if json.Unmarshal(raw, &r) != nil {
		return claudelane.Outcome{Class: "parse_error", Result: string(raw)}
	}
	return claudelane.Outcome{
		Class:    classFromStop(r.StopReason, r.Output),
		Result:   r.Output,
		NumTurns: r.Steps,
	}
}

// ── Cascade door ──────────────────────────────────────────────────────────

// cascadeResult mirrors offload-harness's core.Result --json output (verified
// read-only: internal/core/types.go). Deferred:true is the structured DEFER the
// honest-defer contract turns into a relegation. `result` is the task payload.
type cascadeResult struct {
	OK       bool            `json:"ok"`
	Deferred bool            `json:"deferred"`
	Reason   string          `json:"reason"`
	Data     json.RawMessage `json:"result"`
	Partial  string          `json:"partial"`
}

// RunCascade shells out to the offload-harness binary (cascade door) as a black
// box: `<bin> <verb> - --json`, feeding the prompt on STDIN. The harness
// self-routes its own model tier, so no --model is passed (that flag is only for
// the harness's `nim` remote path). Fail-open like Run.
func RunCascade(ctx context.Context, bin, verb, prompt, question string, timeoutSec int) (claudelane.Outcome, []byte, error) {
	// `-` reads the input text from STDIN; --json returns the full result object.
	args := []string{verb, "-", "--json"}
	// triage requires a --question; default to a generic pass/fail gate prompt so
	// a verify-gate node always has one even if the caller didn't supply it.
	if verb == "triage" {
		q := question
		if q == "" {
			q = "Does the provided content satisfy the stated verification requirement?"
		}
		args = append(args, "--question", q)
	}
	out, errb, runErr := runCmdStdin(ctx, bin, args, prompt, timeoutSec)
	if runErr != nil {
		if len(out) > 0 {
			return parseCascade(out), out, nil // nonzero exit that still produced JSON
		}
		return spawnErr(runErr, errb), nil, nil
	}
	return parseCascade(out), out, nil
}

// parseCascade maps the offload-harness core.Result → an outcome_class.
// S3R-1: a structured DEFER (Deferred:true) → `deferred` (relegation), NEVER a
// false ok and NEVER an error. ok:true → ok (payload carried as content).
// ok:false non-deferred → api_error (a genuine local failure, re-lane-eligible).
// Unparseable stdout → parse_error.
func parseCascade(raw []byte) claudelane.Outcome {
	var r cascadeResult
	if json.Unmarshal(raw, &r) != nil {
		return claudelane.Outcome{Class: "parse_error", Result: string(raw)}
	}
	if r.Deferred {
		reason := r.Reason
		if reason == "" {
			reason = "local cascade deferred (no reason given)"
		}
		return claudelane.Outcome{Class: "deferred", Result: reason}
	}
	if r.OK {
		content := string(r.Data)
		if content == "" {
			content = r.Partial
		}
		return claudelane.Outcome{Class: "ok", Result: content}
	}
	// ok:false and not deferred — a genuine local failure (a tier error, not an
	// honest defer). Not ok; carries the reason for the receipt.
	reason := r.Reason
	if reason == "" {
		reason = r.Partial
	}
	return claudelane.Outcome{Class: "api_error", Result: reason}
}

// ── Shared spawn machinery ────────────────────────────────────────────────

// runCmd runs bin with args (no stdin), with the codexlane timeout + tree-kill
// discipline. Returns stdout, stderr, and the exec error (nil on clean exit).
func runCmd(ctx context.Context, bin string, args []string, timeoutSec int) (stdout, stderr []byte, err error) {
	return runCmdStdin(ctx, bin, args, "", timeoutSec)
}

// runCmdStdin is runCmd with an stdin string fed to the child. Shared by both
// doors so the WaitDelay + Windows taskkill /T /F tree-kill is identical.
func runCmdStdin(ctx context.Context, bin string, args []string, stdin string, timeoutSec int) (stdout, stderr []byte, err error) {
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != "" {
		cmd.Stdin = bytes.NewBufferString(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// F15 interim tree-kill (same as codexlane): a WaitDelay so a wedged child is
	// force-terminated; on Windows a process-tree kill so no orphan lingers.
	cmd.WaitDelay = 10 * time.Second
	if runtime.GOOS == "windows" {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
	}
	runErr := cmd.Run()
	return out.Bytes(), errb.Bytes(), runErr
}

// spawnErr builds the fail-open spawn_error Outcome from an exec error + stderr.
func spawnErr(runErr error, errb []byte) claudelane.Outcome {
	msg := runErr.Error()
	if len(errb) > 0 {
		msg += ": " + string(errb)
	}
	return claudelane.Outcome{Class: "spawn_error", Result: msg}
}
