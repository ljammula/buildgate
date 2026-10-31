package requestdriver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func challengeArgsFixture(t *testing.T) (dataDir string, r *request.Request, ticket request.Ticket) {
	t.Helper()
	dataDir = t.TempDir()
	r = &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Bug: true, Source: request.Source{Kind: request.SourceText}}
	if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, r.ID), "spec.md"), []byte("## Acceptance criteria\n\n- AC-1: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := request.HashFile(dataDir, r.ID, "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{"spec.md": sum}
	specPath := filepath.Join(request.Dir(dataDir, r.ID), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir, r, request.Ticket{Index: 1, SpecPath: specPath}
}

func TestCarvedTicketBuildsNeitherChallengeNorRefuseTheBugOptOut(t *testing.T) {
	dataDir, r, ticket := challengeArgsFixture(t)
	plain, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !requestdrivertestHas(plain, "-challenge") || !requestdrivertestHas(plain, "-bug") {
		t.Fatalf("an ordinary ticket 1 of a bug request lacks -challenge or -bug: %v", plain)
	}
	ticket.Order = &request.TicketOrder{SourceRunID: "mixed-1", Groups: [][]string{{"db/"}}, Group: 1, First: 1}
	carved, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-challenge", "-bug"} {
		if requestdrivertestHas(carved, flag) {
			t.Errorf("a carved ticket's build argv has %s: %v", flag, carved)
		}
	}
}

func TestChangedApprovedSpecIsNoChallengeNotAFailedBuild(t *testing.T) {
	dataDir, r, ticket := challengeArgsFixture(t)
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, r.ID), "spec.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("a spec changed since approval stopped the build: %v", err)
	}
	if requestdrivertestHas(args, "-challenge") {
		t.Fatalf("a changed spec was still given to the challenger: %v", args)
	}
}

// A spec whose criteria carry no AC-n label (a handed-over spec of bullets)
// gives the challenger each criterion by the ID the host accepts in a test
// file's header; one whose criteria are labelled is given as it is.
func TestChallengerIsGivenTheIDOfEachUnlabelledCriterion(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		want       []string
		criteria   string
	}{
		{"bullets", "# Thing\n\n## Wanted behaviour\n\n- A limit can be set.\n- A negative limit is rejected.\n",
			[]string{"# Acceptance criteria by ID", "\nAC-1: A limit can be set.\n", "\nAC-2: A negative limit is rejected.\n"}, "2"},
		{"labelled", "## Acceptance criteria\n\n1. AC-1: When x, the system shall y.\n", nil, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, r, ticket := challengeArgsFixture(t)
			if err := os.WriteFile(filepath.Join(request.Dir(dataDir, r.ID), "spec.md"), []byte(tc.spec), 0o600); err != nil {
				t.Fatal(err)
			}
			sum, err := request.HashFile(dataDir, r.ID, "spec.md")
			if err != nil {
				t.Fatal(err)
			}
			r.ApprovedSHA256 = map[string]string{"spec.md": sum}
			args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.ReadFile(requestdrivertest.ArgValue(args, "-challenge-spec"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(input), tc.spec) {
				t.Errorf("the challenger's input does not start with the approved spec:\n%s", input)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(input), w) {
					t.Errorf("the challenger's input lacks %q:\n%s", w, input)
				}
			}
			if tc.want == nil && strings.Contains(string(input), "# Acceptance criteria by ID") {
				t.Errorf("a spec with labelled criteria got the ID block:\n%s", input)
			}
			if got := requestdrivertest.ArgValue(args, "-challenge-criteria"); got != tc.criteria {
				t.Errorf("-challenge-criteria = %s, want %s", got, tc.criteria)
			}
		})
	}
}

func requestdrivertestHas(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// Only files a run launches are mounted into the round and set aside by
// diff_scope: a file held without a derivation header is neither.
func TestHeldChallengeFilesLeaveOutFilesHeldAndNotRun(t *testing.T) {
	dataDir, r, ticket := challengeArgsFixture(t)
	hostDir := filepath.Join(dataDir, "runs", "r1", "challenge", "0")
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rec := &run.Run{ID: "r1", Challenge: &run.ChallengeRecord{Files: []run.ChallengeFile{
		{Path: "a_test.go", SHA256: strings.Repeat("a", 64), HostPath: filepath.Join(hostDir, "a_test.go"), Criterion: "AC-1"},
		{Path: "b_test.go", SHA256: strings.Repeat("b", 64), HostPath: filepath.Join(hostDir, "b_test.go"), NotRun: "no valid derivation header"},
	}}}
	if err := rec.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	ticket.Challenged, ticket.ChallengeRunID = true, "r1"
	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(requestdrivertest.ArgValue(args, "-challenge-held"))
	if err != nil {
		t.Fatalf("no held manifest: %v (%v)", err, args)
	}
	if !strings.Contains(string(manifest), "a_test.go") || strings.Contains(string(manifest), "b_test.go") {
		t.Fatalf("manifest = %s, want only the file that carries a header", manifest)
	}
}
