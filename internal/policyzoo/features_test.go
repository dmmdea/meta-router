package policyzoo

import "testing"

func TestExtractFeatures(t *testing.T) {
	prompt := "Refactor the parser.\n1. run the tests in pkg/parse/parse_test.go\n2) deploy the fix\n```go\ncode here\n```\n"
	f := Extract(prompt)
	if f.Chars != len(prompt) {
		t.Errorf("Chars=%d want %d", f.Chars, len(prompt))
	}
	if f.CodeFences != 1 {
		t.Errorf("CodeFences=%d want 1", f.CodeFences)
	}
	if f.NumberedSteps != 2 {
		t.Errorf("NumberedSteps=%d want 2", f.NumberedSteps)
	}
	if f.ToolVerbs < 3 { // refactor, run, tests, deploy — word-boundary matches
		t.Errorf("ToolVerbs=%d want >=3", f.ToolVerbs)
	}
	if f.FileRefs != 1 {
		t.Errorf("FileRefs=%d want 1", f.FileRefs)
	}
	if f.Score() != f.CodeFences+f.NumberedSteps+f.ToolVerbs+f.FileRefs {
		t.Error("Score formula drifted")
	}
}

func TestExtractEmpty(t *testing.T) {
	f := Extract("")
	if f.Score() != 0 || f.Chars != 0 {
		t.Errorf("empty prompt must score 0, got %+v", f)
	}
}
