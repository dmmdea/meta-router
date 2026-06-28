// Package goldset loads labeled (prompt -> expected skill ids) cases.
package goldset

import (
	"bufio"
	"encoding/json"
	"os"
)

type Case struct {
	Prompt string   `json:"prompt"`
	Expect []string `json:"expect"`
}

func Load(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c Case
		if json.Unmarshal(line, &c) != nil {
			continue
		}
		out = append(out, c)
	}
	return out, sc.Err()
}
