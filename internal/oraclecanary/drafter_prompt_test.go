package oraclecanary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// drafterPrompt returns everything the oracle drafter can send the model:
// its script and every draft_acceptance_oracles.*.prompt.md template the
// script loads.
func drafterPrompt(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "agent", "pi", "scripts")
	templates, err := filepath.Glob(filepath.Join(dir, "draft_acceptance_oracles.*.prompt.md"))
	if err != nil || len(templates) == 0 {
		t.Fatalf("oracle drafter prompt templates: %v, found %d", err, len(templates))
	}
	var prompt strings.Builder
	for _, path := range append([]string{filepath.Join(dir, "draft_acceptance_oracles.py")}, templates...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		prompt.Write(raw)
	}
	return prompt.String()
}

// The drafter prompt tells the model which oracle file names to use; the canary
// refuses any other name (found live 2026-09-20: the prompt's old example
// "oracle_001.go" produced a draft the canary rejected). This pins the prompt's
// name shapes to Classify.
func TestDrafterPromptNamesAreClassified(t *testing.T) {
	prompt := drafterPrompt(t)
	for shape, name := range map[string]string{
		"oracle_NNN_test.go": "oracle_001_test.go",
		// Python drafting (2026-09-24): the host now accepts Go and
		// Python drafts (requireDraftableEcosystem in
		// cmd/factoryd/oracle_draft_job.go), so this shape is offered too.
		"test_oracle_NNN.py": "test_oracle_001.py",
	} {
		if !strings.Contains(prompt, shape) {
			t.Errorf("drafter prompt no longer mentions %q", shape)
		}
		if _, isTest, err := Classify(name); err != nil || !isTest {
			t.Errorf("Classify(%q) = isTest %v, err %v; the prompt's shape %q must be a recognised oracle test", name, isTest, err, shape)
		}
	}
	// JS/Dart oracle drafting is still not supported: the host refuses these.
	for _, shape := range []string{"NNN_oracle_test.py", "NNN.oracle.test.ts", "NNN_test.dart"} {
		if strings.Contains(prompt, shape) {
			t.Errorf("drafter prompt still offers %q, but the host refuses JS/Dart drafts", shape)
		}
	}
	if !strings.Contains(prompt, "TestOracle<Something>") {
		t.Error("drafter prompt must require Go test functions named TestOracle*")
	}
	if !strings.Contains(prompt, "NO pytest, NO unittest") {
		t.Error("drafter prompt must require Python test functions with no framework (see PythonStdlibCommand)")
	}
	if strings.Contains(prompt, "oracle_001.go") {
		t.Error("drafter prompt still suggests the unrecognised name oracle_001.go")
	}
}

// A timed-out draft is only salvageable if the manifest is on disk, so the
// prompt must ask for it FIRST (7 of 8 live timeouts had it written last).
func TestDrafterPromptAsksForManifestFirst(t *testing.T) {
	prompt := drafterPrompt(t)
	if !strings.Contains(prompt, "Write {manifest_name} FIRST") {
		t.Error("drafter prompt must ask for the manifest first")
	}
	if strings.Contains(prompt, "When you are done, write a file named {manifest_name}") {
		t.Error("drafter prompt still asks for the manifest last")
	}
}
