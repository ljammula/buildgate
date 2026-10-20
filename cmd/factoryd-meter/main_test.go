package main

import (
	"io"
	"log"
	"strings"
	"testing"
)

func TestRunRequiresLedgerRoot(t *testing.T) {
	err := run(nil, log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "-ledger-root is required") {
		t.Fatalf("run() error = %v, want the missing -ledger-root refusal", err)
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	if err := run([]string{"-ledger-root", t.TempDir(), "-tls"}, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("run accepted an unknown flag")
	}
}
