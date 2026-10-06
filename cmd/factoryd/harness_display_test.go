package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// TestStatusRoleLinesShowEachRolesHarness: `factoryd status` names the harness
// each role runs under beside its model and thinking level.
func TestStatusRoleLinesShowEachRolesHarness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	config := `routes:
  local:
    allow_no_credential: true
    upstream: https://model-a.example.invalid
models:
  luna:
    id: gpt-5.6-luna
    api: openai-completions
    routes: [local]
roles:
  execution:
    model: luna
    harness: pifork
  review:
    model: luna
    allow_shared_model: true
`
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rolesStatusLines(path), "\n")
	want := "execution: luna (gpt-5.6-luna, harness pifork)\nreview: luna (gpt-5.6-luna, harness pi)"
	if got != want {
		t.Errorf("rolesStatusLines() = %q, want %q", got, want)
	}
}

// TestRelayModelEffortLineNamesTheAttemptsHarness: `factoryd watch` shows the
// harness an attempt ran under next to its model, and nothing for an attempt
// recorded before Attempt.Harness existed.
func TestRelayModelEffortLineNamesTheAttemptsHarness(t *testing.T) {
	got := relayModelEffortLine([]run.Attempt{
		{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", Harness: "pifork", RelayReasoningEffort: "medium"},
		{Kind: "spec_conformity", RelayWorkerModelID: "gpt-5.6-luna", RelayReasoningEffort: "max"},
	})
	want := "build gpt-5.6-luna via pifork (effort medium) · spec_conformity gpt-5.6-luna (effort max)"
	if got != want {
		t.Errorf("relayModelEffortLine() = %q, want %q", got, want)
	}
}
