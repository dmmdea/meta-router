package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/quotasig"
)

// parityPair is one (lane,window) drop-vs-poll comparison.
type parityPair struct {
	Lane    string    `json:"lane"`
	Window  string    `json:"window"`
	DropPct float64   `json:"drop_pct"`
	PollPct float64   `json:"poll_pct"`
	Delta   float64   `json:"delta"`
	DropTS  time.Time `json:"drop_ts"`
	PollTS  time.Time `json:"poll_ts"`
}

type parityReport struct {
	Window   string       `json:"window"`
	Pairs    []parityPair `json:"pairs"`
	Unpaired []string     `json:"unpaired,omitempty"`
	MaxAbs   float64      `json:"max_abs_delta"`
	Note     string       `json:"note"`
}

// buildParity pairs the LATEST drop-origin and poll-origin rows per
// (lane,window) within the lookback. Pure; unit-tested directly.
func buildParity(rows []quotasig.TraceRow, since time.Time) parityReport {
	type latest struct {
		drop, poll *quotasig.TraceRow
	}
	byKey := map[string]*latest{}
	for i := range rows {
		r := rows[i]
		if r.TS.Before(since) {
			continue
		}
		k := r.Lane + "|" + r.Window
		l, ok := byKey[k]
		if !ok {
			l = &latest{}
			byKey[k] = l
		}
		switch r.Origin {
		case "drop":
			if l.drop == nil || r.TS.After(l.drop.TS) {
				l.drop = &rows[i]
			}
		case "oauth_poll", "wham_poll":
			if l.poll == nil || r.TS.After(l.poll.TS) {
				l.poll = &rows[i]
			}
		}
	}
	rep := parityReport{Note: "drop-vs-poll divergence per (lane,window); the W1 soak gate reads max_abs_delta (target <=2pp typical). A report, not a gate: exit 0 always."}
	for k, l := range byKey {
		switch {
		case l.drop != nil && l.poll != nil:
			p := parityPair{Lane: l.drop.Lane, Window: l.drop.Window,
				DropPct: l.drop.UsedPct, PollPct: l.poll.UsedPct,
				Delta: l.poll.UsedPct - l.drop.UsedPct, DropTS: l.drop.TS, PollTS: l.poll.TS}
			rep.Pairs = append(rep.Pairs, p)
			if a := math.Abs(p.Delta); a > rep.MaxAbs {
				rep.MaxAbs = a
			}
		default:
			rep.Unpaired = append(rep.Unpaired, k)
		}
	}
	return rep
}

func loadTraceRows(path string) []quotasig.TraceRow {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []quotasig.TraceRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var r quotasig.TraceRow
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Lane != "" {
			out = append(out, r)
		}
	}
	return out
}

// runQuotaParity is the soak-gate reading: how far apart are the tee and the
// pollers on the same windows?
func runQuotaParity(args []string) error {
	fs := flag.NewFlagSet("quota-parity", flag.ExitOnError)
	lookback := fs.Duration("window", 24*time.Hour, "lookback for pairing trace rows")
	_ = fs.Parse(args)
	now := time.Now().UTC()
	rep := buildParity(loadTraceRows(quotaTracePath()), now.Add(-*lookback))
	rep.Window = lookback.String()
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
