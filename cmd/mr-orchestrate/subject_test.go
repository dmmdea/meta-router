package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/profiles"
)

func TestPickSubjectSingleProfileIsInert(t *testing.T) {
	l := ledger.Open(filepath.Join(t.TempDir(), "l.json"))
	reg := profiles.Registry{"claude": {{Subject: "default", Provisioned: true}}}
	sel := pickSubject(reg, l, "claude", time.Now().UTC())
	// Empty Subject (not the literal "default") keeps receipts byte-identical
	// to pre-W2 — the field is omitempty, so single-account machines never
	// grow a subject column.
	if sel.Subject != "" || sel.Home != "" || sel.RotationFrom != "" {
		t.Fatalf("single profile must yield the inert (empty) selection, got %+v", sel)
	}
}

func TestPickSubjectRotatesOffExhausted(t *testing.T) {
	l := ledger.Open(filepath.Join(t.TempDir(), "l.json"))
	now := time.Now().UTC()
	reset := now.Add(2 * time.Hour)
	l.ObserveProvider("claude", ledger.Win5h, 97, reset, now)                     // default exhausted
	l.ObserveProviderSubject("claude", "acct2", ledger.Win5h, 5, reset, now)      // acct2 open
	reg := profiles.Registry{"claude": {
		{Subject: "default", Home: "", Provisioned: true},
		{Subject: "acct2", Home: "C:/x/acct2", Provisioned: true},
	}}
	sel := pickSubject(reg, l, "claude", now)
	if sel.Subject != "acct2" {
		t.Fatalf("must rotate to the open account, got %+v", sel)
	}
	if sel.Home != "C:/x/acct2" {
		t.Fatalf("rotated selection must carry the profile home, got %q", sel.Home)
	}
	if sel.RotationFrom != "default" || sel.RotationReason == "" {
		t.Fatalf("rotation provenance must be set, got %+v", sel)
	}
}
