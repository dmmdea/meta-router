package main

import (
	"fmt"
	"sort"
	"strings"
)

// knownLanes is the closed set --exclude accepts (the laneStates keys plus
// local). A typo like "claud" must be an error, not a silent no-op that leaves
// the lane selectable.
var knownLanes = map[string]bool{"claude": true, "codex": true, "copilot": true, "glm": true, "local": true}

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
			return nil, fmt.Errorf("exclude: unknown lane %q (valid: %s)", r, strings.Join(keys, "|"))
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}
