package copilotlane

import (
	"slices"
	"strings"
	"testing"
)

func validReq() RunReq {
	return RunReq{Prompt: "p", Model: "auto", Home: "/tmp/h", Token: "tok"}
}

func TestBuildArgsPinsTheIsolationSurface(t *testing.T) {
	args, err := BuildArgs(validReq())
	if err != nil {
		t.Fatal(err)
	}
	// Every isolation flag is load-bearing: deny-all tools (text-dispatch
	// lane), no ask_user (headless), no builtin MCPs (a live spawn without
	// them loaded the operator's desktop config and spawned MCP servers).
	for _, must := range []string{"--deny-tool", "--no-ask-user", "--disable-builtin-mcps", "--output-format", "json", "--no-color", "-p", "--model"} {
		if !slices.Contains(args, must) {
			t.Fatalf("args missing %q: %v", must, args)
		}
	}
}

func TestBuildArgsRequiredFields(t *testing.T) {
	for name, mut := range map[string]func(*RunReq){
		"model":  func(r *RunReq) { r.Model = "" },
		"prompt": func(r *RunReq) { r.Prompt = "" },
		"home":   func(r *RunReq) { r.Home = "" },
		"token":  func(r *RunReq) { r.Token = "" },
	} {
		r := validReq()
		mut(&r)
		if _, err := BuildArgs(r); err == nil {
			t.Fatalf("missing %s must be a config error", name)
		}
	}
}

// Every permission-widening / session-bleeding / egress flag is rejected in
// both the split and single-token forms. Table-driven over the REAL list so
// adding a forbidden flag automatically extends the test.
func TestBuildArgsForbiddenExtras(t *testing.T) {
	for _, flag := range forbiddenExtraPrefixes {
		for _, form := range []string{flag, flag + "=x"} {
			r := validReq()
			r.Extra = []string{form}
			if _, err := BuildArgs(r); err == nil {
				t.Fatalf("Extra %q must be rejected", form)
			} else if !strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("Extra %q rejection must say forbidden: %v", form, err)
			}
		}
	}
}

// A benign passthrough (e.g. a log dir override) survives — the guard is a
// denylist of known-dangerous flags, not a lockout of R11 operator intent.
func TestBuildArgsBenignExtraSurvives(t *testing.T) {
	r := validReq()
	r.Extra = []string{"--log-dir", "/tmp/logs"}
	args, err := BuildArgs(r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--log-dir") {
		t.Fatalf("benign extra dropped: %v", args)
	}
}
