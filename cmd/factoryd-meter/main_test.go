package main

import (
	"fmt"
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

func TestWriteStatfsPrintsFreeAndTotalBytes(t *testing.T) {
	var out strings.Builder
	if err := writeStatfs(&out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var free, total uint64
	if n, err := fmt.Sscanf(out.String(), "free=%d total=%d", &free, &total); err != nil || n != 2 {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	if total == 0 || free > total {
		t.Errorf("free %d, total %d", free, total)
	}
	if err := writeStatfs(io.Discard, "/no/such/path"); err == nil {
		t.Error("statfs of a missing path succeeded")
	}
}
