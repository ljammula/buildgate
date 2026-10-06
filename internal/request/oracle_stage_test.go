package request

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRequestOracle writes request-level oracle/<name> files (RUN_COMMAND.txt
// gets a valid command, others a stub).
func writeRequestOracle(t *testing.T, dataDir, id string, names ...string) {
	t.Helper()
	dir := filepath.Join(Dir(dataDir, id), RequestOracleDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		content := "package x\n"
		if strings.HasSuffix(name, "_test.go") {
			// A real oracle test: approval now refuses a directory the runtime
			// canary could not make fail (it needs a TestOracle* function).
			content = "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
		}
		if name == TicketOracleRunCommandFilename {
			content = "go test ./.oracle/...\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRequestOracleManifest(t, dataDir, id, names)
}

// writeRequestOracleManifest gives the test files among names a MANIFEST.json
// (one entry each, criterion i -> file i) and rewrites spec.md with that many
// numbered criteria, as approval now requires for a request-level oracle. It
// does nothing when names has no test file or already names a MANIFEST.json.
func writeRequestOracleManifest(t *testing.T, dataDir, id string, names []string) {
	t.Helper()
	var tests []string
	for _, n := range names {
		if n == ManifestFileName {
			return
		}
		if strings.HasSuffix(n, "_test.go") {
			tests = append(tests, n)
		}
	}
	if len(tests) == 0 {
		return
	}
	spec := "# Spec\n\n## Problem\n\nx\n\n## Acceptance criteria\n\n"
	var entries []string
	for i, n := range tests {
		spec += fmt.Sprintf("%d. Criterion %d\n", i+1, i+1)
		entries = append(entries, fmt.Sprintf(`{"criterion": "Criterion %d", "oracle_file": %q, "criterion_index": %d}`, i+1, n, i+1))
	}
	spec += "\n## Risks\n\nNone.\n"
	dir := Dir(dataDir, id)
	if err := os.WriteFile(filepath.Join(dir, specFileName), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RequestOracleDirName, ManifestFileName), []byte("["+strings.Join(entries, ",")+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, dataDir, id string) *Request {
	t.Helper()
	r, err := Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newDraftOraclesRequest(t *testing.T, dataDir, id string, state State) *Request {
	t.Helper()
	r := newApprovableRequest(t, dataDir, id, state, false)
	r.DraftOracles = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestApproveSpecRoutesByDraftOracles: the flag-less request goes straight to
// planning (byte-identical to before the oracle stage existed: no
// draft_oracles key in request.json, planning as the next state); a request
// with DraftOracles goes to oracle_drafting.
func TestApproveSpecRoutesByDraftOracles(t *testing.T) {
	t.Run("flag-less", func(t *testing.T) {
		dataDir := t.TempDir()
		newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)
		r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != StatePlanning {
			t.Errorf("State = %q, want planning", r.State)
		}
		b, err := os.ReadFile(filepath.Join(Dir(dataDir, "req-1"), "request.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "draft_oracles") || strings.Contains(string(b), "oracle_draft") {
			t.Errorf("flag-less request.json carries oracle keys: %s", b)
		}
		last := r.History[len(r.History)-1]
		if last.To != StatePlanning || last.Reason != "approved" {
			t.Errorf("history tail = %+v", last)
		}
	})
	t.Run("flag set", func(t *testing.T) {
		dataDir := t.TempDir()
		newDraftOraclesRequest(t, dataDir, "req-1", StateSpecReview)
		r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != StateOracleDrafting {
			t.Errorf("State = %q, want oracle_drafting", r.State)
		}
		if _, ok := r.ApprovedSHA256[specFileName]; !ok {
			t.Errorf("spec.md not pinned: %v", r.ApprovedSHA256)
		}
	})
}

// TestOracleStageTransitions walks the state graph and the halt paths.
func TestOracleStageTransitions(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StateOracleDrafting
	if err := r.CompleteOracleDrafting(OracleDraft{Status: OracleDraftFailed, Detail: "boom"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.State != StateOracleReview || r.OracleDraft == nil || r.OracleDraft.Status != OracleDraftFailed {
		t.Fatalf("state=%q draft=%+v", r.State, r.OracleDraft)
	}
	if err := r.CompleteOracleDrafting(OracleDraft{Status: OracleDrafted}, fixedNow); err == nil {
		t.Error("CompleteOracleDrafting from oracle_review must be illegal")
	}
	if err := r.ApproveSpecToOracles("a", fixedNow); err == nil {
		t.Error("ApproveSpecToOracles from oracle_review must be illegal")
	}
	r.State = StateOracleDrafting
	if err := r.CompleteOracleDrafting(OracleDraft{Status: "bogus"}, fixedNow); err == nil {
		t.Error("an unknown draft status must be refused")
	}
	for _, from := range []State{StateOracleDrafting, StateOracleReview} {
		h := New("req-2", "/w", "w", Source{Kind: SourceText}, fixedNow)
		h.State = from
		if err := h.Halt("x", fixedNow); err != nil {
			t.Fatalf("Halt from %s: %v", from, err)
		}
		if h.HaltedFrom() != from {
			t.Errorf("HaltedFrom = %q", h.HaltedFrom())
		}
		if err := h.ResumeDrafting(StateOracleDrafting, "op", "", fixedNow); err != nil {
			t.Fatalf("ResumeDrafting from halted (was %s): %v", from, err)
		}
		if h.State != StateOracleDrafting || h.Error != "" {
			t.Errorf("state=%q error=%q", h.State, h.Error)
		}
	}
}

// TestApproveOracleReviewSkipsAbsentOrEmptyDir: no oracle/ or an empty one
// goes to planning with nothing pinned beyond what was already pinned.
func TestApproveOracleReviewSkipsAbsentOrEmptyDir(t *testing.T) {
	for name, mk := range map[string]func(dataDir string){
		"absent": func(string) {},
		"empty": func(dataDir string) {
			if err := os.MkdirAll(filepath.Join(Dir(dataDir, "req-1"), RequestOracleDirName), 0o750); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
			mk(dataDir)
			r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.State != StatePlanning {
				t.Errorf("State = %q, want planning", r.State)
			}
			for rel := range r.ApprovedSHA256 {
				if isRequestOracleRelPath(rel) {
					t.Errorf("skip pinned %q", rel)
				}
			}
			// The API path also accepts a skip with no oracle files to show.
			dataDir2 := t.TempDir()
			newDraftOraclesRequest(t, dataDir2, "req-1", StateOracleReview)
			mk2 := mk
			mk2(dataDir2)
			if _, err := ApproveShown(dataDir2, "req-1", "alice", fixedNow, map[string]string{}); err != nil {
				t.Errorf("ApproveShown skip: %v", err)
			}
		})
	}
}

// TestApproveOracleReviewRefusesUnusableDirectory: refusals leave the request
// exactly as it was.
func TestApproveOracleReviewRefusesUnusableDirectory(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, dataDir string)
		want  string
	}{
		"no RUN_COMMAND.txt": {func(t *testing.T, d string) { writeRequestOracle(t, d, "req-1", "oracle_test.go") }, TicketOracleRunCommandFilename},
		"run command misses mount": {func(t *testing.T, d string) {
			writeRequestOracle(t, d, "req-1", "oracle_test.go", TicketOracleRunCommandFilename)
			if err := os.WriteFile(filepath.Join(Dir(d, "req-1"), "oracle", TicketOracleRunCommandFilename), []byte("go test ./...\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, TicketOracleMountPath},
		"canary cannot support it: helper test without TestOracle*": {func(t *testing.T, d string) {
			writeRequestOracle(t, d, "req-1", "oracle_test.go", "helpers_test.go", TicketOracleRunCommandFilename)
			if err := os.WriteFile(filepath.Join(Dir(d, "req-1"), "oracle", "helpers_test.go"), []byte("package x\n\nfunc helper() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "TestOracle"},
		"subdirectory": {func(t *testing.T, d string) {
			writeRequestOracle(t, d, "req-1", TicketOracleRunCommandFilename)
			if err := os.MkdirAll(filepath.Join(Dir(d, "req-1"), "oracle", "sub"), 0o750); err != nil {
				t.Fatal(err)
			}
		}, "subdirectory"},
		"symlink": {func(t *testing.T, d string) {
			writeRequestOracle(t, d, "req-1", TicketOracleRunCommandFilename)
			if err := os.Symlink("/etc/hostname", filepath.Join(Dir(d, "req-1"), "oracle", "link_test.go")); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
			tc.setup(t, dataDir)
			_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			got := mustLoad(t, dataDir, "req-1")
			if got.State != StateOracleReview || len(got.ApprovedSHA256) != 0 || got.ApprovedBy != "" {
				t.Errorf("refused approval changed the request: %+v", got)
			}
		})
	}
}

// TestApproveOracleReviewPinsAndVerifies: request-level oracle files are
// hash-pinned, and a post-approval edit or deletion is caught by
// VerifyApprovedHashes.
func TestApproveOracleReviewPinsAndVerifies(t *testing.T) {
	dataDir := t.TempDir()
	newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	writeRequestOracle(t, dataDir, "req-1", TicketOracleRunCommandFilename, "oracle_test.go")
	r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"oracle/RUN_COMMAND.txt", "oracle/oracle_test.go", "oracle/MANIFEST.json"} {
		if r.ApprovedSHA256[rel] == "" {
			t.Errorf("%s not pinned: %v", rel, r.ApprovedSHA256)
		}
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("clean verify: %v", err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), "oracle", "oracle_test.go"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovedHashes(dataDir, r); err == nil || !strings.Contains(err.Error(), "oracle/oracle_test.go") {
		t.Errorf("edit not detected: %v", err)
	}
}

// TestApproveShownRefusesUncoveredRequestOracle: the API-facing approval at
// oracle_review is refused when the client's expected map does not cover the
// request-level oracle files (the console cannot show them); a client that
// lists them approves; the unconditional CLI path approves.
func TestApproveShownRefusesUncoveredRequestOracle(t *testing.T) {
	setup := func(t *testing.T) (string, map[string]string) {
		dataDir := t.TempDir()
		newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
		writeRequestOracle(t, dataDir, "req-1", TicketOracleRunCommandFilename, "oracle_test.go")
		full := map[string]string{}
		for _, rel := range []string{"oracle/RUN_COMMAND.txt", "oracle/oracle_test.go", "oracle/MANIFEST.json"} {
			h, err := HashFile(dataDir, "req-1", rel)
			if err != nil {
				t.Fatal(err)
			}
			full[rel] = h
		}
		return dataDir, full
	}
	for name, expected := range map[string]func(full map[string]string) map[string]string{
		"nil":       func(map[string]string) map[string]string { return nil },
		"empty":     func(map[string]string) map[string]string { return map[string]string{} },
		"spec only": func(map[string]string) map[string]string { return map[string]string{"spec.md": "x"} },
		"partial oracle": func(f map[string]string) map[string]string {
			return map[string]string{"oracle/RUN_COMMAND.txt": f["oracle/RUN_COMMAND.txt"]}
		},
	} {
		t.Run("refused "+name, func(t *testing.T) {
			dataDir, full := setup(t)
			_, err := ApproveShown(dataDir, "req-1", "alice", fixedNow, expected(full))
			if !errors.Is(err, ErrOracleNotShown) {
				t.Fatalf("err = %v, want ErrOracleNotShown", err)
			}
			if got := mustLoad(t, dataDir, "req-1"); got.State != StateOracleReview || len(got.ApprovedSHA256) != 0 {
				t.Errorf("refusal changed the request: %+v", got)
			}
		})
	}
	t.Run("full map approves", func(t *testing.T) {
		dataDir, full := setup(t)
		if _, err := ApproveShown(dataDir, "req-1", "alice", fixedNow, full); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("CLI path approves unconditionally", func(t *testing.T) {
		dataDir, _ := setup(t)
		if _, err := Approve(dataDir, "req-1", "alice", fixedNow, nil); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRejectOracleReview: back to oracle_drafting, reason on Rejections and
// NOT in request.md, oracle/* snapshotted, feedback carries the reasons.
func TestRejectOracleReview(t *testing.T) {
	dataDir := t.TempDir()
	newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	writeRequestOracle(t, dataDir, "req-1", TicketOracleRunCommandFilename, "oracle_test.go")
	before, err := os.ReadFile(TextPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}

	r, err := Reject(dataDir, "req-1", "bob", "test asserts the wrong thing", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateOracleDrafting {
		t.Errorf("State = %q, want oracle_drafting", r.State)
	}
	after, _ := os.ReadFile(TextPath(dataDir, "req-1"))
	if string(after) != string(before) {
		t.Errorf("request.md changed by an oracle rejection: %q", after)
	}
	if len(r.Rejections) != 1 || r.Rejections[0].FromState != StateOracleReview || r.Rejections[0].Reason != "test asserts the wrong thing" {
		t.Fatalf("Rejections = %+v", r.Rejections)
	}
	revs, err := ListRevisions(dataDir, "req-1")
	if err != nil || len(revs) != 1 {
		t.Fatalf("revisions = %+v, %v", revs, err)
	}
	if revs[0].FromState != StateOracleReview || len(revs[0].Files) != 3 || revs[0].Files[0] != "oracle/MANIFEST.json" {
		t.Errorf("revision = %+v, want the oracle files snapshotted", revs[0])
	}
	if fb := OracleFeedback(r); !strings.Contains(fb, "test asserts the wrong thing") || !strings.Contains(fb, "bob") {
		t.Errorf("OracleFeedback = %q", fb)
	}
	if !strings.HasSuffix(OracleFeedbackPath(dataDir, "req-1"), OracleFeedbackFileName) || OracleFeedbackPath(dataDir, "req-1") == TextPath(dataDir, "req-1") {
		t.Errorf("feedback path must be its own file, got %s", OracleFeedbackPath(dataDir, "req-1"))
	}
}

// TestRejectOracleReviewWithoutOracleDirStillSucceeds: like plan_review's own
// regression, a missing directory must never block the reject.
func TestRejectOracleReviewWithoutOracleDirStillSucceeds(t *testing.T) {
	dataDir := t.TempDir()
	newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	if _, err := Reject(dataDir, "req-1", "bob", "redo it", fixedNow); err != nil {
		t.Fatal(err)
	}
}

// TestOracleFeedbackOnlyCarriesOracleStageRejections: spec and plan reasons
// never leak into the drafter's feedback.
func TestOracleFeedbackOnlyCarriesOracleStageRejections(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.Rejections = []Rejection{
		{By: "a", Reason: "spec reason", FromState: StateSpecReview},
		{By: "b", Reason: "oracle reason", FromState: StateOracleReview},
		{By: "c", Reason: "plan reason", FromState: StatePlanReview},
	}
	fb := OracleFeedback(r)
	if !strings.Contains(fb, "oracle reason") || strings.Contains(fb, "spec reason") || strings.Contains(fb, "plan reason") {
		t.Errorf("OracleFeedback = %q", fb)
	}
}

// TestSpecFeedbackOnlyCarriesSpecStageRejections and
// TestPlanFeedbackOnlyCarriesPlanStageRejections are
// TestOracleFeedbackOnlyCarriesOracleStageRejections' own siblings:
// SpecFeedback/PlanFeedback must be just as stage-scoped as OracleFeedback
// already is.
func TestSpecFeedbackOnlyCarriesSpecStageRejections(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.Rejections = []Rejection{
		{By: "a", Reason: "spec reason", FromState: StateSpecReview},
		{By: "b", Reason: "oracle reason", FromState: StateOracleReview},
		{By: "c", Reason: "plan reason", FromState: StatePlanReview},
	}
	fb := SpecFeedback(r)
	if !strings.Contains(fb, "spec reason") || strings.Contains(fb, "oracle reason") || strings.Contains(fb, "plan reason") {
		t.Errorf("SpecFeedback = %q", fb)
	}
}

func TestPlanFeedbackOnlyCarriesPlanStageRejections(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.Rejections = []Rejection{
		{By: "a", Reason: "spec reason", FromState: StateSpecReview},
		{By: "b", Reason: "oracle reason", FromState: StateOracleReview},
		{By: "c", Reason: "plan reason", FromState: StatePlanReview},
	}
	fb := PlanFeedback(r)
	if !strings.Contains(fb, "plan reason") || strings.Contains(fb, "oracle reason") || strings.Contains(fb, "spec reason") {
		t.Errorf("PlanFeedback = %q", fb)
	}
}
