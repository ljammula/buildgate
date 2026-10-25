package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

func TestStagePreviousSpecStagesTheCurrentSpecOnlyForAHandedOverRequest(t *testing.T) {
	dataDir := t.TempDir()
	r := requestdrivertest.HandedOverRequest(t, dataDir, requestdrivertest.CanonicalValidSpec)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(requestdrivertest.CanonicalValidSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	path, err := stagePreviousSpec(dataDir, r, scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != requestdrivertest.CanonicalValidSpec || path != filepath.Join(scratch, previousDraftFileName) {
		t.Fatalf("staged %q at %s (%v)", got, path, err)
	}

	// A send-back after the import itself halted: no spec.md was written,
	// so the file as handed over is the document to revise.
	if err := os.Remove(requestdriver.RequestSpecPath(dataDir, r.ID)); err != nil {
		t.Fatal(err)
	}
	path, err = stagePreviousSpec(dataDir, r, scratch, "")
	if err != nil {
		t.Fatalf("with no spec.md: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != requestdrivertest.CanonicalValidSpec {
		t.Fatalf("staged %q, want the handed-over file", got)
	}

	r.SpecImported = false
	if path, err := stagePreviousSpec(dataDir, r, scratch, ""); err != nil || path != "" {
		t.Fatalf("a drafted request staged %q (%v); want nothing", path, err)
	}
}

func TestDraftInputFilesArgsAndContainerPaths(t *testing.T) {
	host := draftInputFiles{feedback: "/host/s/feedback.md", previousDraft: "/host/s/previous-draft.md"}
	got := strings.Join(host.inContainer("/workspace/.factory-spec-draft").args(), " ")
	want := "--feedback-file /workspace/.factory-spec-draft/feedback.md --previous-draft-file /workspace/.factory-spec-draft/previous-draft.md"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if args := (draftInputFiles{}).args(); len(args) != 0 {
		t.Errorf("no files gave args %v", args)
	}
	if got := strings.Join((draftInputFiles{keepStructure: true}).args(), " "); got != "--keep-structure" {
		t.Errorf("keepStructure args = %q, want --keep-structure", got)
	}
}

func TestResolveSubmitSpecFile(t *testing.T) {
	dir := t.TempDir()
	specFile := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(specFile, []byte(requestdrivertest.CanonicalValidSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	// No request text: the spec's Problem section stands in.
	spec, trailing, err := resolveSubmitSpecFile(submitParams{specFile: specFile})
	if err != nil || spec != requestdrivertest.CanonicalValidSpec || len(trailing) != 1 || trailing[0] != "Refunds can be double-processed on retry." {
		t.Fatalf("got trailing %q (%v)", trailing, err)
	}
	// A request text given: it is kept.
	_, trailing, err = resolveSubmitSpecFile(submitParams{specFile: specFile, trailingText: []string{"Idempotent", "refunds"}})
	if err != nil || strings.Join(trailing, " ") != "Idempotent refunds" {
		t.Fatalf("got trailing %q (%v)", trailing, err)
	}
	// No spec file: untouched.
	spec, trailing, err = resolveSubmitSpecFile(submitParams{trailingText: []string{"x"}})
	if err != nil || spec != "" || len(trailing) != 1 {
		t.Fatalf("got %q, %q (%v)", spec, trailing, err)
	}
	if _, _, err := resolveSubmitSpecFile(submitParams{specFile: filepath.Join(dir, "absent.md")}); err == nil || !strings.Contains(err.Error(), "read -spec-file") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveSubmitSpecFileUsesStructuralFallbacks(t *testing.T) {
	dir := t.TempDir()
	// A missing/empty Problem section uses the first heading's text.
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte(strings.Replace(requestdrivertest.CanonicalValidSpec, "Refunds can be double-processed on retry.\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	fallbackSpec, trailing, err := resolveSubmitSpecFile(submitParams{specFile: empty})
	if err != nil || fallbackSpec == "" || len(trailing) != 1 || trailing[0] != "Spec" {
		t.Fatalf("fallback request text = %q (%v), want the first heading", trailing, err)
	}
	noHeading := filepath.Join(dir, "no-heading.md")
	if err := os.WriteFile(noHeading, []byte("---\ntitle: Example\nitems:\n  - hidden\n...\n\nFirst request line.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, trailing, err = resolveSubmitSpecFile(submitParams{specFile: noHeading})
	if err != nil || len(trailing) != 1 || trailing[0] != "First request line." {
		t.Fatalf("front-matter fallback = %q (%v), want first non-blank content line", trailing, err)
	}
	leadingBlank := filepath.Join(dir, "leading-blank.md")
	if err := os.WriteFile(leadingBlank, []byte("\n\nFirst request line.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, trailing, err = resolveSubmitSpecFile(submitParams{specFile: leadingBlank})
	if err != nil || len(trailing) != 1 || trailing[0] != "First request line." {
		t.Fatalf("leading-blank fallback = %q (%v), want first non-blank content line", trailing, err)
	}
	fencedHeading := filepath.Join(dir, "fenced-heading.md")
	if err := os.WriteFile(fencedHeading, []byte("~~~markdown\n# Not the title\n~~~\n\n# Real title\n\n- criterion\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, trailing, err = resolveSubmitSpecFile(submitParams{specFile: fencedHeading})
	if err != nil || len(trailing) != 1 || trailing[0] != "Real title" {
		t.Fatalf("fenced-heading fallback = %q (%v), want the first structural heading", trailing, err)
	}
}
