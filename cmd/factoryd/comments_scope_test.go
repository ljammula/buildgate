package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// specReviewWithSkeleton is a request in spec_review whose spec.md has every
// required heading.
func specReviewWithSkeleton(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, "req-1"), []byte(requestdrivertest.TwoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func TestRejectMainCommentsFile(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := specReviewWithSkeleton(t)
	comments := filepath.Join(t.TempDir(), "comments.json")
	body := `[{"path":"spec.md","section":"## Scope","quote":"The /refunds endpoint only.","note":"add /refunds/{id}"},
	          {"path":"spec.md","section":"## Acceptance criteria","item":2,"note":"which account?"}]`
	if err := os.WriteFile(comments, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rejectMain(dp, []string{"-comments-file", comments, "-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("rejectMain -comments-file: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecDrafting || len(loaded.Rejections) != 1 {
		t.Fatalf("State = %q, rejections = %+v", loaded.State, loaded.Rejections)
	}
	rej := loaded.Rejections[0]
	if len(rej.Anchors) != 2 || rej.Anchors[0].Quote != "The /refunds endpoint only." || rej.Anchors[1].Item != 2 {
		t.Errorf("anchors = %+v", rej.Anchors)
	}
	scope := loaded.RedraftScope(request.StateSpecReview)
	if scope == nil || strings.Join(scope.Files["spec.md"], "|") != "## Scope|## Acceptance criteria" {
		t.Errorf("scope = %+v, want the two commented sections", scope)
	}
}

func TestRejectMainCommentsFileRefusals(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := specReviewWithSkeleton(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.json", `[{"path":"spec.md","section":"## Scope","note":"n"}]`)
	for name, args := range map[string][]string{
		"with -to":          {"-comments-file", good, "-to", "plan", "-data-dir", dataDir, "req-1"},
		"unknown field":     {"-comments-file", write("typo.json", `[{"path":"spec.md","qoute":"x","note":"n"}]`), "-data-dir", dataDir, "req-1"},
		"trailing data":     {"-comments-file", write("trail.json", `[{"path":"spec.md","note":"n"}] []`), "-data-dir", dataDir, "req-1"},
		"empty list":        {"-comments-file", write("empty.json", `[]`), "-data-dir", dataDir, "req-1"},
		"missing file":      {"-comments-file", filepath.Join(dir, "none.json"), "-data-dir", dataDir, "req-1"},
		"passage not there": {"-comments-file", write("moved.json", `[{"path":"spec.md","section":"## Risks","quote":"The /refunds endpoint only.","note":"n"}]`), "-data-dir", dataDir, "req-1"},
	} {
		if err := rejectMain(dp, args); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if r, err := request.Load(dataDir, "req-1"); err != nil || r.State != request.StateSpecReview {
		t.Errorf("a refused rejection moved the request: %+v, %v", r, err)
	}
}

// TestSpecDraftJobStagesTheCurrentSpecForAScopedRedraft: a spec_review
// rejection with comments hands the drafting job the reviewed spec.md and the
// sections to rewrite; one without comments hands it neither.
func TestSpecDraftJobStagesTheCurrentSpecForAScopedRedraft(t *testing.T) {
	dataDir := specReviewWithSkeleton(t)
	anchors := []request.RejectionAnchor{
		{Path: "spec.md", Section: "## Scope", Note: "n"},
		{Path: "spec.md", Section: "## Risks", Note: "n"},
	}
	if _, err := request.RejectAnchored(dataDir, "req-1", "bob", "", anchors, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	previous, err := stagePreviousSpec(dataDir, r, scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(previous); string(got) != requestdrivertest.TwoCriteriaSpec {
		t.Errorf("staged previous draft = %q, want the reviewed spec.md", got)
	}
	rewrite, err := stageRewriteSections(r.RedraftScope(request.StateSpecReview), scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(rewrite)
	want := `[{"path":"spec.md","section":"## Scope"},{"path":"spec.md","section":"## Risks"}]` + "\n"
	if string(got) != want {
		t.Errorf("rewrite sections = %q, want %q", got, want)
	}
	args := draftInputFiles{previousDraft: previous, rewriteSections: rewrite}.inContainer("/workspace/.factory-spec-draft").args()
	if !strings.Contains(strings.Join(args, " "), "--rewrite-sections-file /workspace/.factory-spec-draft/rewrite-sections.json") {
		t.Errorf("args = %v", args)
	}

	// A plain rejection (no comments) is a full redraft: nothing staged.
	plainDir := specReviewWithSkeleton(t)
	if _, err := request.RejectAnchored(plainDir, "req-1", "bob", "shorter", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	plain, _ := request.Load(plainDir, "req-1")
	if path, err := stagePreviousSpec(plainDir, plain, scratch, ""); err != nil || path != "" {
		t.Errorf("a plain rejection staged %q (%v)", path, err)
	}
	if path, err := stageRewriteSections(plain.RedraftScope(request.StateSpecReview), scratch, ""); err != nil || path != "" {
		t.Errorf("a plain rejection staged sections %q (%v)", path, err)
	}
}
