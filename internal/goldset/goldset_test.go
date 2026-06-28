package goldset

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.jsonl")
	os.WriteFile(p, []byte(
		`{"prompt":"QA test my web app","expect":["skills:gstack-qa"]}`+"\n"+
			`{"prompt":"plan a new multi-phase project","expect":["skills:gsd-new-project"]}`+"\n"), 0o600)
	cs, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Expect[0] != "skills:gstack-qa" {
		t.Fatalf("bad load: %+v", cs)
	}
}

func TestLoadSkipsBlankLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.jsonl")
	os.WriteFile(p, []byte(
		"\n"+
			`{"prompt":"brainstorm a feature","expect":["skills:brainstorming"]}`+"\n"+
			"\n"), 0o600)
	cs, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("expected 1 case, got %d: %+v", len(cs), cs)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.jsonl")
	os.WriteFile(p, []byte(""), 0o600)
	cs, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatalf("expected 0 cases, got %d", len(cs))
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/g.jsonl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
