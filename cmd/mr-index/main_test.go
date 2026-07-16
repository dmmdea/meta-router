package main

import "testing"

// The endpoint default is deliberately EMPTY: a hardcoded host default is what
// broke this on a second machine (the shared settings.json pinned one box's
// port on every box). Empty means "resolve for whatever machine this is" —
// retrievers.ResolveEndpoints handles env / machine-local file / failover chain.
func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := parseArgs([]string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.cmd != "build" || cfg.endpoint != "" || cfg.force {
		t.Fatalf("bad defaults: %+v", cfg)
	}
}

// An explicitly passed endpoint must still be honored verbatim.
func TestParseArgs_EndpointFlagIsHonored(t *testing.T) {
	cfg, err := parseArgs([]string{"refresh", "-endpoint", "http://10.0.0.1:9999", "-force"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.endpoint != "http://10.0.0.1:9999" || !cfg.force {
		t.Fatalf("flag not honored: %+v", cfg)
	}
}

func TestParseArgs_RejectsUnknownCmd(t *testing.T) {
	if _, err := parseArgs([]string{"frobnicate"}); err == nil {
		t.Fatal("expected error on unknown subcommand")
	}
}

func TestParseArgs_EmptyArgv(t *testing.T) {
	if _, err := parseArgs(nil); err == nil {
		t.Fatal("expected error on empty argv")
	}
}
