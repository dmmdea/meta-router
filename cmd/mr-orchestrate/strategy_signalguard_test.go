package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	"github.com/dmmdea/meta-router/internal/orch/strategy"
)

// S3R-10a — solo == sync run is a COMMITTED regression test (a permanent pin, not
// just an evidence transcript). A `solo` template dispatch of a prompt must
// produce the SAME terminal artifact CONTENT that a plain doRun of that prompt
// produces. This pins that the strategy engine can NEVER silently degrade the
// fast path: the 1-node solo DAG is byte-for-byte the sync path.
//
// No cloud spend: a deterministic echo node runner (the nodeDispatch seam) stands
// in for the real dispatch. The SAME fake drives both sides, so any divergence is
// a real regression in the solo DAG's terminal-content plumbing, not fake drift.
func TestSignalGuardSoloEqualsSyncRun(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())

	const prompt = "reply ok"
	const class = "latency-iteration"

	// A deterministic echo: whatever doRun would emit, the fake emits the same
	// bytes for the same prompt, and writes the ONE tagged receipt real doRun
	// writes (S3R-4). It never calls a cloud lane.
	echo := func(opts runOpts) string { return `{"result":"` + opts.Prompt + `","lane":"local"}` }
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		io.WriteString(out, echo(opts))
		// One tagged receipt per node (only when driven as a strategy step).
		if opts.DispatchID != "" {
			_ = dispatch.Append(dispatchPath(), dispatch.Record{
				TS: time.Now().UTC(), Lane: "local", OutcomeClass: "ok", Origin: "strategy",
				DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
			})
		}
		return 0, nil
	}

	// ── Sync path: a plain doRun of the prompt (what `run` does). ──
	var syncOut strings.Builder
	syncCode, err := nodeDispatch(runOpts{Prompt: prompt, Class: class, Lane: "local", Origin: "cli"}, &syncOut)
	if err != nil || syncCode != 0 {
		t.Fatalf("sync doRun must succeed: code=%d err=%v", syncCode, err)
	}
	syncContent := syncOut.String()

	// ── Strategy path: the solo template, driven end-to-end through the executor. ──
	ir := strategy.Solo(prompt, class)
	if ir.Name != "solo" || len(ir.Steps) != 1 {
		t.Fatalf("Solo must be the 1-node DAG, got name=%q steps=%d", ir.Name, len(ir.Steps))
	}
	id := "solo-eq-sync"
	dir := statepaths.StrategyDir(id)
	if err := strategy.WriteInitial(dir, ir, id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := strategy.Execute(dir, prodNodeRunner(id, prodAlternatives), prodResolve, prodAlternatives,
		strategy.ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return time.Now().UTC() }, nil); err != nil {
		t.Fatalf("solo dispatch must execute cleanly: %v", err)
	}

	st, err := strategy.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "done" {
		t.Fatalf("a solo dispatch of a plain prompt must complete done, got %q", st.State)
	}
	// The terminal artifact content is the solo DAG's answer.
	art, err := strategy.ReadArtifact(st.ResultRef)
	if err != nil {
		t.Fatalf("solo terminal artifact must be readable: %v", err)
	}

	// THE PIN: the solo DAG's terminal content is byte-identical to the sync run's.
	if art.Content != syncContent {
		t.Fatalf("S3R-10a REGRESSION: solo terminal content must equal the sync run's.\n solo: %q\n sync: %q", art.Content, syncContent)
	}
}

// S3R-10a corollary: the Solo template is exactly the 1-node fast path — deps=[],
// the goal as the sole instruction, class passed through, role worker. Pinning
// the shape guards against a future template edit that quietly makes solo more
// than a pass-through (which would break the byte-identity above).
func TestSignalGuardSoloTemplateIsPurePassThrough(t *testing.T) {
	ir := strategy.Solo("do the thing", "hard-repo")
	if len(ir.Steps) != 1 {
		t.Fatalf("solo must be exactly 1 node, got %d", len(ir.Steps))
	}
	s := ir.Steps[0]
	if s.ID != 0 || s.Instruction != "do the thing" || s.Class != "hard-repo" || len(s.Deps) != 0 {
		t.Fatalf("solo node must be a pure pass-through {id:0, instruction:goal, class, deps:[]}, got %+v", s)
	}
	if err := strategy.Validate(ir); err != nil {
		t.Fatalf("solo must validate: %v", err)
	}
}
