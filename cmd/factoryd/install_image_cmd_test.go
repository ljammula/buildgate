package main

import (
	"context"
	"strings"
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// An accepted run whose input carried an approved sandbox block, but whose
// result has no install image record for it, was judged by a worker older
// than the install image: it is quarantined for the operator.
func TestAnAcceptedRunWithoutItsInstallRecordIsQuarantined(t *testing.T) {
	const hash = "abc123"
	accepted := func(install *run.InstallImage) workflow.RunWorkflowResult {
		r := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
		r.Install = install
		return r
	}
	for name, tc := range map[string]struct {
		sandboxSHA string
		result     workflow.RunWorkflowResult
		quarantine bool
	}{
		"no record":                  {hash, accepted(nil), true},
		"record of another block":    {hash, accepted(&run.InstallImage{SandboxSHA256: "other", Image: "img"}), true},
		"record with no image":       {hash, accepted(&run.InstallImage{SandboxSHA256: hash}), true},
		"its record":                 {hash, accepted(&run.InstallImage{SandboxSHA256: hash, Image: "img"}), false},
		"no block":                   {"", accepted(nil), false},
		"not accepted is left as is": {hash, allowingRunWorkflowResultForTest(run.StateQuarantined, "deadbeef"), false},
	} {
		t.Run(name, func(t *testing.T) {
			got := requireCurrentWorker(nil, nil, tc.sandboxSHA, tc.result)
			notRun := false
			for _, g := range got.GateResults {
				notRun = notRun || g.InstallNotRun()
			}
			if tc.quarantine != (got.State == run.StateQuarantined && notRun) {
				t.Errorf("state %s, install-not-run result %v; want quarantined %v", got.State, notRun, tc.quarantine)
			}
		})
	}
	r := &run.Run{State: run.StateQuarantined, GateResults: requireCurrentWorker(nil, nil, hash, accepted(nil)).GateResults}
	if doc := handoff.Build(r, t.TempDir()); doc.Next != handoff.BinOperator {
		t.Errorf("handoff Next = %q, want operator", doc.Next)
	}
}

// recordingInstallImages records the evictions `factoryd sandbox evict` asks for.
type recordingInstallImages struct {
	refusingInstallImages
	repoKeys []string
	keeps    []int
}

func (r *recordingInstallImages) Evict(_ context.Context, _, repoKey string, keep int) (int, error) {
	r.repoKeys, r.keeps = append(r.repoKeys, repoKey), append(r.keeps, keep)
	return 2, nil
}

var _ workflow.InstallImages = (*recordingInstallImages)(nil)

func TestSandboxEvictRemovesTheRepositorysInstallImages(t *testing.T) {
	repo, dataDir, _ := sandboxRepo(t, sandboxFactoryYML)
	dp := newTestDeps(t)
	images := &recordingInstallImages{}
	fakeDockerOf(dp).installImagesFn = func() workflow.InstallImages { return images }
	var out strings.Builder
	if err := sandboxEvict(dp, newSandboxFlags(), []string{"-repo", repo, "-data-dir", dataDir}, &out); err != nil {
		t.Fatal(err)
	}
	if err := sandboxEvict(dp, newSandboxFlags(), []string{"-data-dir", dataDir}, &out); err != nil {
		t.Fatal(err)
	}
	if len(images.repoKeys) != 2 || images.repoKeys[0] != repoKeyOf(repo) || images.repoKeys[1] != "" || images.keeps[0] != 0 {
		t.Errorf("evictions = %q keep %v, want the repository's then every repository's, keeping none", images.repoKeys, images.keeps)
	}
	if !strings.Contains(out.String(), "removed 2 install image(s)") {
		t.Errorf("output = %q", out.String())
	}
	if err := sandboxEvict(dp, newSandboxFlags(), []string{"-sha256", "x"}, &out); err == nil {
		t.Error("evict accepted -sha256")
	}
}
