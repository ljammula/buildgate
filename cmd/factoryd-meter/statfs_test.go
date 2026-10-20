//go:build unix

package main

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

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
