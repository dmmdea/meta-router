package glmlane

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// S2R-6 cadence hygiene: the GLM ban class fires on PATTERN, not volume (XDA
// precedent at 0%/9% quota) — so the lane self-paces: max concurrency 1 (an
// exclusive lock held for the dispatch) plus a minimum inter-dispatch
// interval with jitter (default ~20–40s, config). This is real-account
// protection, not an artificial brake — R14-compliant for the same reason the
// 1313 latch is. Fail-open discipline: filesystem anomalies never block a
// dispatch; only a genuinely HELD lock (another dispatch in flight beyond
// LockWait) relegates.

const (
	lockName         = "glm-lane.lock"
	stampName        = "glm-last-dispatch"
	defaultPollEvery = 500 * time.Millisecond
)

type PaceConfig struct {
	MinInterval time.Duration // floor between dispatch STARTS
	Jitter      time.Duration // uniform random addition to the floor
	LockWait    time.Duration // max wait on the concurrency lock before relegating
	LockStale   time.Duration // steal locks older than this (a genuinely DEAD holder)
	// Heartbeat is how often a HELD lock's mtime is refreshed (A2R-#5). A live
	// holder keeps its lock fresh, so LockStale only ever reclaims a holder that
	// has genuinely STOPPED refreshing (crashed) — a long (>LockStale) but live
	// dispatch is never stolen. Zero uses defaultHeartbeat.
	Heartbeat time.Duration
	// Poll is the wait-loop poll interval (test seam). Zero uses defaultPollEvery.
	Poll time.Duration
}

const defaultHeartbeat = 30 * time.Second

// DefaultPace builds the config-seconds into a PaceConfig. A live lock holder
// heartbeats its mtime every 30s, so LockStale (90s = 3 missed beats) only
// reclaims a holder that has genuinely died — even though a GLM dispatch can
// run >20min (API_TIMEOUT_MS=50min). LockWait comfortably EXCEEDS LockStale so
// a waiter can always reclaim a genuinely-dead lock before it relegates, and
// never false-relegates a live long-running holder (A2R-#5).
func DefaultPace(minSec, jitterSec int64) PaceConfig {
	return PaceConfig{
		MinInterval: time.Duration(minSec) * time.Second,
		Jitter:      time.Duration(jitterSec) * time.Second,
		Heartbeat:   defaultHeartbeat,
		LockStale:   90 * time.Second, // 3 missed heartbeats
		LockWait:    5 * time.Minute,  // > LockStale: a dead lock is always reclaimable before we give up
	}
}

// Pace acquires the lane's concurrency lock and enforces the inter-dispatch
// interval, then stamps the dispatch start. now/sleep are injected so the
// wait math is deterministic under test. The returned unlock releases the
// lock (callers defer it around the dispatch); err is non-nil ONLY when the
// lock wait is exhausted — the caller relegates (RS5), never hangs.
func Pace(dir string, pc PaceConfig, now func() time.Time, sleep func(time.Duration)) (unlock func(), waited time.Duration, err error) {
	noop := func() {}
	if merr := os.MkdirAll(dir, 0o755); merr != nil {
		return noop, 0, nil // fail-open: pacing must never brick the lane
	}
	lock := filepath.Join(dir, lockName)
	poll := pc.Poll
	if poll <= 0 {
		poll = defaultPollEvery
	}
	deadline := now().Add(pc.LockWait)
	for {
		f, oerr := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if oerr == nil {
			fmt.Fprintf(f, "%d %s", os.Getpid(), now().UTC().Format(time.RFC3339))
			f.Close()
			break
		}
		if !os.IsExist(oerr) {
			return noop, 0, nil // fs anomaly (permissions, etc.): fail open
		}
		// A2R-#5: reclaim ONLY a genuinely-stale lock — one whose mtime has aged
		// past LockStale, i.e. the holder stopped heartbeating (crashed). A live
		// holder refreshes its mtime, so this never steals a long-running
		// dispatch. Re-checked every poll so a lock that goes stale WHILE we wait
		// is reclaimed rather than timing out first.
		if fi, serr := os.Stat(lock); serr == nil && now().Sub(fi.ModTime()) > pc.LockStale {
			_ = os.Remove(lock) // dead holder — steal
			continue
		}
		if now().After(deadline) {
			return noop, 0, fmt.Errorf("glm lane busy: another dispatch holds %s (max concurrency 1, S2R-6)", lock)
		}
		sleep(poll)
	}

	// Heartbeat the held lock's mtime so a live (even >LockStale) dispatch is
	// never mistaken for a crashed holder and stolen — the S2R-6 concurrency-1
	// ban guard depends on the lock actually holding for the whole dispatch.
	// Real wall-clock time (the dispatch outlives injected test time); the
	// goroutine is stopped by unlock and cannot outlive it (A2R-#5).
	hb := pc.Heartbeat
	if hb <= 0 {
		hb = defaultHeartbeat
	}
	stop := make(chan struct{})
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	go func() {
		defer hbDone.Done()
		t := time.NewTicker(hb)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := time.Now()
				_ = os.Chtimes(lock, now, now) // refresh mtime; fail-open on races
			}
		}
	}()
	var unlockOnce sync.Once
	unlock = func() {
		unlockOnce.Do(func() {
			close(stop)   // stop heartbeating FIRST
			hbDone.Wait() // ensure the goroutine has exited before we remove the file
			_ = os.Remove(lock)
		})
	}

	stamp := filepath.Join(dir, stampName)
	if b, rerr := os.ReadFile(stamp); rerr == nil {
		if last, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); perr == nil {
			interval := pc.MinInterval
			if pc.Jitter > 0 {
				interval += time.Duration(rand.Int64N(int64(pc.Jitter) + 1))
			}
			// A stamp in the future is clock skew — fail open rather than
			// sleeping on garbage.
			if el := now().Sub(last); el >= 0 && el < interval {
				waited = interval - el
				sleep(waited)
			}
		}
	}
	_ = os.WriteFile(stamp, []byte(now().UTC().Format(time.RFC3339Nano)), 0o644) // fail-open
	return unlock, waited, nil
}
