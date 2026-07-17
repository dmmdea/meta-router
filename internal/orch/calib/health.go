package calib

import (
	"os"
	"time"
)

// TraceHealth is the E6 health verdict over the quota-trace calibration corpus.
// The trace feeds Fit (capacity learning): a missing/empty/stalled trace means
// caps silently never learn — this makes it loud (status surfaces it).
type TraceHealth struct {
	Exists bool      `json:"exists"`
	Rows   int       `json:"rows"`
	LastTS time.Time `json:"last_ts"`
	Stale  bool      `json:"stale"`
}

// AssessTrace reads the trace (via the same Load the fitter uses — one parser,
// one truth) and grades it. Stale = missing, empty, or last row older than
// staleAfter. Fail-quiet on read errors: an unreadable trace reads as absent
// (the alarm still fires — that IS the signal).
func AssessTrace(path string, now time.Time, staleAfter time.Duration) TraceHealth {
	h := TraceHealth{}
	if _, err := os.Stat(path); err == nil {
		h.Exists = true
	}
	rows := Load(path)
	h.Rows = len(rows)
	for _, r := range rows {
		if r.TS.After(h.LastTS) {
			h.LastTS = r.TS
		}
	}
	h.Stale = h.Rows == 0 || now.Sub(h.LastTS) > staleAfter
	return h
}
