package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunMainWithReadyRefusesTheRetiredConformityFlags: the spec-conformity
// review is retired, and with it the two flags that fed it. A run passed
// either is refused as an unknown flag, not run with the flag ignored.
func TestRunMainWithReadyRefusesTheRetiredConformityFlags(t *testing.T) {
	for _, retired := range [][]string{
		{"-conformity-policy", "advisory"},
		{"-spec-acceptance-criteria", "criteria.md"},
	} {
		t.Run(retired[0], func(t *testing.T) {
			dp := newTestDeps(t)
			dir := t.TempDir()
			spec := filepath.Join(dir, "spec.md")
			if err := os.WriteFile(spec, []byte("# fixture spec\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := runMainWithReady(dp, context.Background(), append([]string{"-ticket", "t", "-workspace", dir, "-spec", spec}, retired...), nil)
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: "+retired[0]) {
				t.Fatalf("runMainWithReady(%v) = %v, want the unknown-flag refusal", retired, err)
			}
		})
	}
}
