package main

import "testing"

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := parseArgs([]string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.cmd != "build" || cfg.endpoint != "http://127.0.0.1:11436" {
		t.Fatalf("bad defaults: %+v", cfg)
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
