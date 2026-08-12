package policyeval

import (
	"math/rand"
	"testing"
)

// The frontier's lower-bound claim, as a runnable property rather than a
// number quoted in a changelog.
//
// The artifact, the stderr warning, the struct field and the package doc all
// tell the operator that while frontier_free_unmeasured_tasks is nonzero,
// EVERY point on the curve is a lower bound — the max-budget point included.
// That claim was strengthened from "the low-budget points" on the strength of
// a 4000-table probe that lived in a scratch directory and was cited in the
// CHANGELOG but never committed, so the strongest statistical statement the
// eval stack makes could not be re-run by the person reading it (review round
// 6). It is committed here.
//
// The property: build a TRUTH table where every task is measured on both
// sides, then HOLE it by deleting the free-lane cells of some tasks. The
// holed curve must be pointwise ≤ the truth curve at every budget, never
// above it. Two mechanisms make it so, and the test would catch either
// breaking: the imputed free base of 0 depresses the base, and it also caps
// that task's withClaude through the max(claude, free) clamp.
func TestFrontierIsPointwiseLowerBoundUnderFreeHoles(t *testing.T) {
	rng := rand.New(rand.NewSource(20260812))
	const tables = 4000

	withHoles, understatedAtMax, violations := 0, 0, 0
	for i := 0; i < tables; i++ {
		nTasks := 1 + rng.Intn(6)
		tasks := make([]string, nTasks)
		truth, holed := NewTable(), NewTable()
		holedAny := false
		for j := range tasks {
			id := string(rune('a' + j))
			tasks[j] = id
			claudePass := rng.Intn(2) == 0
			freePass := rng.Intn(2) == 0
			truth.Add(id, lc("claude"), claudePass)
			truth.Add(id, lc("local"), freePass)
			holed.Add(id, lc("claude"), claudePass)
			// Hole the free side of roughly half the tasks. A task with NO
			// cell at all is excluded from both curves (unmeasuredTasks), so
			// only the free side is removed here.
			if rng.Intn(2) == 0 {
				holed.Add(id, lc("local"), freePass)
			} else {
				holedAny = true
			}
		}

		truthPts, _, truthFree := Frontier(truth, tasks)
		holedPts, _, holedFree := Frontier(holed, tasks)
		if truthFree != 0 {
			t.Fatalf("table %d: the truth table has no free-side holes, got %d", i, truthFree)
		}
		if holedAny != (holedFree > 0) {
			t.Fatalf("table %d: holed free cells=%v but freeUnmeasuredTasks=%d", i, holedAny, holedFree)
		}
		if !holedAny {
			continue
		}
		withHoles++

		// Pointwise comparison at every budget the holed curve reports. The
		// holed curve can be SHORTER (its budget axis spans sweepable tasks),
		// so compare against the truth curve at the same budget index.
		for b := range holedPts {
			if b >= len(truthPts) {
				t.Fatalf("table %d: holed curve is longer than the truth curve (%d > %d)", i, len(holedPts), len(truthPts))
			}
			if holedPts[b].PassRate > truthPts[b].PassRate+1e-12 {
				violations++
				t.Errorf("table %d budget %d: holed pass_rate %.6f EXCEEDS truth %.6f — the curve is not a lower bound",
					i, b, holedPts[b].PassRate, truthPts[b].PassRate)
			}
		}
		// The claim that specifically needed strengthening: the MAX-budget
		// point can understate too, so it is not a measurement either.
		hMax, tMax := holedPts[len(holedPts)-1], truthPts[len(truthPts)-1]
		if hMax.PassRate < tMax.PassRate-1e-12 {
			understatedAtMax++
		}
	}

	if violations != 0 {
		t.Fatalf("%d upper-bound violations across %d tables — the lower-bound claim is false", violations, tables)
	}
	if withHoles == 0 {
		t.Fatal("no table exercised a free-side hole: the property was never tested")
	}
	// Guard the guard: if the max-budget point were always exact, the
	// strengthened wording ("the max-budget point included") would be
	// overclaiming, and this test would be silently vacuous about the very
	// sentence it exists to support.
	if understatedAtMax == 0 {
		t.Fatal("no table understated at MAX budget, so this test does not actually support the claim that the " +
			"max-budget point is a lower bound — the fixture or the wording is wrong")
	}
	t.Logf("%d/%d tables had free-side holes; %d strictly understated at max budget; %d upper-bound violations",
		withHoles, tables, understatedAtMax, violations)
}
