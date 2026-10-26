package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// writeRecheckManifest writes m as a manifest file and returns its path.
func writeRecheckManifest(t *testing.T, m run.RecheckManifest) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recheck.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// heldCopy writes a held copy under run id's review-repro directory.
func heldCopy(t *testing.T, dataDir, id string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(run.ReviewReproductionDir(run.Dir(dataDir, id)), "0", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "repro_test.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

// A recheck manifest is accepted only when every held copy it names is a
// regular file under the quarantined run's review-repro directory, reached
// through no link, with a clean relative reproduction path, a hash, a known
// runner and its test IDs.
func TestRecheckManifestHostPathsMustBeUnderTheQuarantinedRun(t *testing.T) {
	dataDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := heldCopy(t, dataDir, "run-q")
	other := heldCopy(t, dataDir, "run-other")
	link := filepath.Join(filepath.Dir(host), "link_test.go")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	good := run.HeldReproduction{Path: "p/repro_test.go", SHA256: strings.Repeat("a", 64), HostPath: host, TestIDs: []string{"TestRepro"}, Runner: "go-test-json"}
	got, err := loadRecheckManifest(dataDir, writeRecheckManifest(t, run.RecheckManifest{RunID: "run-q", Reproductions: []run.HeldReproduction{good}}))
	if err != nil || len(got) != 1 || got[0].HostPath != host {
		t.Fatalf("a valid manifest = %+v, %v; want it accepted", got, err)
	}
	for name, mutate := range map[string]func(*run.RecheckManifest){
		"another run's copy": func(m *run.RecheckManifest) { m.Reproductions[0].HostPath = other },
		"a link out":         func(m *run.RecheckManifest) { m.Reproductions[0].HostPath = link },
		"a relative path":    func(m *run.RecheckManifest) { m.Reproductions[0].HostPath = "p/repro_test.go" },
		"a dot-dot path": func(m *run.RecheckManifest) {
			m.Reproductions[0].HostPath = filepath.Dir(host) + "/../../../run-other/review-repro/0/p/repro_test.go"
		},
		"the directory itself": func(m *run.RecheckManifest) { m.Reproductions[0].HostPath = filepath.Dir(host) },
		"a missing file": func(m *run.RecheckManifest) {
			m.Reproductions[0].HostPath = filepath.Join(filepath.Dir(host), "gone_test.go")
		},
		"an absolute reproduction path": func(m *run.RecheckManifest) { m.Reproductions[0].Path = "/p/repro_test.go" },
		"a dot-dot reproduction path":   func(m *run.RecheckManifest) { m.Reproductions[0].Path = "p/../../repro_test.go" },
		"an unclean reproduction path":  func(m *run.RecheckManifest) { m.Reproductions[0].Path = "p//repro_test.go" },
		"an option-like path":           func(m *run.RecheckManifest) { m.Reproductions[0].Path = "-p/repro_test.go" },
		"no hash":                       func(m *run.RecheckManifest) { m.Reproductions[0].SHA256 = "" },
		"an unknown runner":             func(m *run.RecheckManifest) { m.Reproductions[0].Runner = "cargo" },
		"no test IDs":                   func(m *run.RecheckManifest) { m.Reproductions[0].TestIDs = nil },
		"a refused finding":             func(m *run.RecheckManifest) { m.Reproductions[0].Refused = "no runner" },
		"an invalid run id":             func(m *run.RecheckManifest) { m.RunID = "../run-q" },
		"no reproduction":               func(m *run.RecheckManifest) { m.Reproductions = nil },
	} {
		m := run.RecheckManifest{RunID: "run-q", Reproductions: []run.HeldReproduction{good}}
		mutate(&m)
		if _, err := loadRecheckManifest(dataDir, writeRecheckManifest(t, m)); err == nil {
			t.Errorf("%s: manifest accepted, want it refused", name)
		}
	}
	unknown := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"run_id":"run-q","reproductions":[],"command":"rm -rf /"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRecheckManifest(dataDir, unknown); err == nil {
		t.Error("a manifest with an unknown field was accepted")
	}
}

// The workflow's decisions reach the run record's findings by index, only
// where the finding there carries the same path; the rechecks are recorded
// as they are.
func TestMergeReviewReproductionsKeysByIndexAndPath(t *testing.T) {
	r := &run.Run{CodeReview: &run.CodeReviewResult{Findings: []run.CodeReviewFinding{
		{Summary: "a", Reproduction: &run.ReviewReproduction{Path: "a_test.go"}},
		{Summary: "b", Reproduction: &run.ReviewReproduction{Path: "b_test.go"}},
		{Summary: "c"},
	}}}
	rechecks := []run.ReviewReproduction{{Path: "old_test.go", Verdict: "passes"}}
	mergeReviewReproductions(r, []run.FindingReproduction{
		{Finding: 0, Reproduction: run.ReviewReproduction{Path: "a_test.go", Verdict: "blocks"}, Disposition: run.DispositionBlocks},
		{Finding: 1, Reproduction: run.ReviewReproduction{Path: "other_test.go", Verdict: "blocks"}, Disposition: run.DispositionBlocks},
		{Finding: 7, Reproduction: run.ReviewReproduction{Path: "x_test.go"}, Disposition: run.DispositionBlocks},
	}, rechecks)
	f := r.CodeReview.Findings
	if f[0].Disposition != run.DispositionBlocks || f[0].Reproduction.Verdict != "blocks" {
		t.Errorf("the first finding = %+v, want the decision", f[0])
	}
	if f[1].Disposition != "" || f[1].Reproduction.Verdict != "" || f[2].Disposition != "" {
		t.Errorf("the other findings = %+v, %+v; want them unchanged", f[1], f[2])
	}
	if len(r.ReviewReproductionRechecks) != 1 || r.ReviewReproductionRechecks[0].Path != "old_test.go" {
		t.Errorf("rechecks = %+v", r.ReviewReproductionRechecks)
	}
}
