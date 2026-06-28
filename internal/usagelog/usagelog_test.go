package usagelog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAppend_TwoLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := Append(p, Record{TsUnix: 1, PromptHash: "h", Surfaced: []string{"skills:a"}, Mode: "hybrid"}); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, Record{TsUnix: 2, Mode: "gated-empty"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var n int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d not valid JSON: %v", n, err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("got %d lines want 2", n)
	}
}

func TestHashPrompt_Stable(t *testing.T) {
	if HashPrompt("x") != HashPrompt("x") || HashPrompt("x") == HashPrompt("y") {
		t.Fatal("hash not stable/distinct")
	}
	const wantX = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" // sha256("x")
	if got := HashPrompt("x"); got != wantX {
		t.Fatalf("HashPrompt(\"x\") = %s, want %s", got, wantX)
	}
}
