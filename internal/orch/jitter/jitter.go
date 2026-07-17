// Package jitter is the slice-4 E4 reset-boundary smoother. admission.go
// supplies a conservative ResumeAt on every denial and explicitly hands retry
// smoothing to slice-4: when several deferred runs share one ResumeAt they
// re-hammer the reset boundary synchronously (thundering herd). RetryAt spreads
// retries uniformly into [resumeAt, resumeAt+window). The truthful resume_at is
// never modified — callers emit retry_at ALONGSIDE it as the scheduling hint.
package jitter

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"math/rand"
	"time"
)

// DefaultWindow is the spread width. A smoothing width (not a capacity or
// threshold constant): 90s spreads a realistic burst of deferred nightshift
// re-dispatches without materially delaying any of them.
const DefaultWindow = 90 * time.Second

// RetryAt returns resumeAt + uniform[0, window). r nil -> the process-global
// source (prod); tests pass a seeded *rand.Rand for determinism.
//
// Use this on a ONE-SHOT emit path (e.g. `mr-orchestrate run`, which prints the
// deferral once and exits): each separate deferred process legitimately wants a
// fresh independent draw. For a POLLED surface, use RetryAtStable instead.
func RetryAt(resumeAt time.Time, window time.Duration, r *rand.Rand) time.Time {
	if window <= 0 {
		return resumeAt
	}
	var n int64
	if r != nil {
		n = r.Int63n(int64(window))
	} else {
		n = rand.Int63n(int64(window))
	}
	return resumeAt.Add(time.Duration(n))
}

// RetryAtStable is RetryAt with the jitter derived deterministically from key,
// so the same (key, resumeAt) always yields the same offset. Use it on POLLED
// surfaces — e.g. strategy_status, which a scheduler reads repeatedly: with the
// plain random RetryAt the hint re-rolls on every read (non-idempotent), so a
// poller sees a different retry_at each call and can never settle on a wake time.
// Seeding on the dispatch/step identity keeps the value stable across reads while
// keeping DISTINCT dispatches decorrelated — the E4 anti-herd spread is preserved
// across the fleet, it just stops flickering within one dispatch.
func RetryAtStable(resumeAt time.Time, window time.Duration, key string) time.Time {
	if window <= 0 {
		return resumeAt
	}
	h := fnv.New64a()
	io.WriteString(h, key)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(resumeAt.UnixNano()))
	h.Write(b[:])
	src := rand.New(rand.NewSource(int64(h.Sum64())))
	return resumeAt.Add(time.Duration(src.Int63n(int64(window))))
}
