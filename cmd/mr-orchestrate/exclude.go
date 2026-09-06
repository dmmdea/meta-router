package main

import (
	"fmt"
	"sort"
	"strings"
)

// knownLanes is the closed set --exclude accepts (the laneStates keys plus
// local). A typo like "claud" must be an error, not a silent no-op that leaves
// the lane selectable.
var knownLanes = map[string]bool{"claude": true, "codex": true, "copilot": true, "glm": true, "local": true,
	// Free-provider lanes (W4) and the "free" GROUP alias that expands to all five.
	"groq": true, "cloudflare": true, "openrouter": true, "nim": true, "gemini": true, "free": true}

// freeGroup expands the "free" alias: --exclude free masks every free-provider
// lane at once (delegate-mode's natural "no third-party" switch).
var freeGroup = []string{"groq", "cloudflare", "openrouter", "nim", "gemini"}

// excludeFlag is a repeatable, comma-tolerant flag.Value:
//
//	--exclude claude --exclude codex   ==   --exclude claude,codex
//
// Values are validated on Set so `flag.ExitOnError` surfaces a typo at parse
// time with the valid names in the message.
type excludeFlag []string

func (e *excludeFlag) String() string { return strings.Join(*e, ",") }

func (e *excludeFlag) Set(v string) error {
	lanes, err := parseExclude(append([]string(*e), strings.Split(v, ",")...))
	if err != nil {
		return err
	}
	*e = lanes
	return nil
}

// parseExclude trims, lower-cases, drops empties, dedupes and sorts; any
// name outside knownLanes is an error naming the valid set.
func parseExclude(raw []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, r := range raw {
		l := strings.ToLower(strings.TrimSpace(r))
		if l == "" {
			continue
		}
		if !knownLanes[l] {
			keys := make([]string, 0, len(knownLanes))
			for k := range knownLanes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("exclude: unknown lane %q (valid: %s)", l, strings.Join(keys, "|"))
		}
		names := []string{l}
		if l == "free" {
			names = freeGroup
		}
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
