package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lockStaleAfter is how old a lock file may get before the next run takes it
// over: a run that was killed mid-refresh must not block refreshes forever.
const lockStaleAfter = 10 * time.Minute

// acquireLock takes <index path>.lock with O_EXCL (pid and time inside, for a
// human). held=true means another run owns it and this one should stand down.
// Two overlapping refreshes used to share one fixed temp name and lose a save
// in 15.6% of runs; the second one now exits clean instead.
func acquireLock(indexPath string) (release func(), held bool, err error) {
	lockPath := indexPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, false, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d started %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			f.Close()
			return func() { os.Remove(lockPath) }, false, nil
		}
		if !os.IsExist(err) {
			return nil, false, err
		}
		fi, serr := os.Stat(lockPath)
		if serr != nil || time.Since(fi.ModTime()) < lockStaleAfter {
			return nil, true, nil // held (or just released: the next run will get it)
		}
		os.Remove(lockPath) // stale: take it over on the next pass
	}
	return nil, true, nil
}

// pruneStaleTemps removes save temp files next to the index that no run can
// still be writing: older than lockStaleAfter. Saves use a unique temp name per
// call, so a run killed mid-save leaves its temp behind and nothing ever reuses
// the name; without this they would accumulate (each is a full index). Called
// with the refresh lock held. Young temps are left alone: a build running
// beside this refresh may own one.
func pruneStaleTemps(indexPath string) {
	base := filepath.Base(indexPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base)) // index.json and index.bin share it
	tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(indexPath), stem+".*.tmp"))
	for _, t := range tmps {
		if fi, err := os.Stat(t); err == nil && !fi.IsDir() && time.Since(fi.ModTime()) > lockStaleAfter {
			os.Remove(t)
		}
	}
}

// releaseLock is set once the lock is held; exit runs it on every path, since
// os.Exit skips defers and a lock left behind would block the next run until
// it goes stale.
var releaseLock = func() {}

func exit(code int) {
	releaseLock()
	os.Exit(code)
}
