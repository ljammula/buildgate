package requestdriver_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// SC-022: a restack's replay run runs no challenge, even for a request whose
// approved spec gives its ticket builds one.
func TestReplayRunCarriesNoChallenge(t *testing.T) {
	f := newRestackFixture(t, 2)
	body := []byte("# Spec\n\n- `Parse` returns a map.\n")
	if err := os.WriteFile(filepath.Join(request.Dir(f.dataDir, f.r.ID), "spec.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if f.r.ApprovedSHA256 == nil {
		f.r.ApprovedSHA256 = map[string]string{}
	}
	f.r.ApprovedSHA256["spec.md"] = hex.EncodeToString(sum[:])
	f.save()
	build, err := requestdriver.BuildRequestBuildArgs(f.dataDir, f.r, f.r.Tickets[1], f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(build, "-challenge") {
		t.Fatalf("a ticket build of this request = %v, want -challenge (the fixture must approve the spec)", build)
	}
	f.pass()
	if len(f.runArgs) != 1 {
		t.Fatalf("replay runs = %d, want 1 (events %v)", len(f.runArgs), f.events)
	}
	for _, flag := range []string{"-challenge", "-challenge-held", "-challenge-round", "-challenge-spec"} {
		if slices.Contains(f.runArgs[0], flag) {
			t.Errorf("the replay run is given %s: %v", flag, f.runArgs[0])
		}
	}
}

// SC-022: a replay run of a challenged ticket gets the ticket's held files as
// set-aside only (-challenge-set-aside): its branch carries their commit, and
// diff_scope must not count them, but nothing runs them.
func TestReplayRunOfAChallengedTicketSetsItsHeldFilesAside(t *testing.T) {
	f := newRestackFixture(t, 2)
	hostDir := filepath.Join(f.dataDir, "runs", "chal-run", "challenge", "0")
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rec := &run.Run{ID: "chal-run", Challenge: &run.ChallengeRecord{Files: []run.ChallengeFile{
		{Path: "a_test.go", SHA256: strings.Repeat("a", 64), HostPath: filepath.Join(hostDir, "a_test.go"), Criterion: "AC-1"},
	}}}
	if err := rec.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	f.r.Tickets[1].Challenged, f.r.Tickets[1].ChallengeRunID = true, "chal-run"
	f.save()
	f.pass()
	if len(f.runArgs) != 1 {
		t.Fatalf("replay runs = %d, want 1 (events %v)", len(f.runArgs), f.events)
	}
	args := f.runArgs[0]
	manifest, err := os.ReadFile(requestdrivertest.ArgValue(args, "-challenge-set-aside"))
	if err != nil {
		t.Fatalf("no set-aside manifest: %v (%v)", err, args)
	}
	if !strings.Contains(string(manifest), "a_test.go") || !strings.Contains(string(manifest), strings.Repeat("a", 64)) {
		t.Errorf("manifest = %s, want the held file and its hash", manifest)
	}
	for _, flag := range []string{"-challenge", "-challenge-held", "-challenge-round", "-challenge-spec"} {
		if slices.Contains(args, flag) {
			t.Errorf("the replay run is given %s: %v", flag, args)
		}
	}
}

// A replay run of a ticket that was never challenged sets nothing aside.
func TestReplayRunOfAnUnchallengedTicketSetsNothingAside(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.pass()
	if len(f.runArgs) != 1 {
		t.Fatalf("replay runs = %d, want 1 (events %v)", len(f.runArgs), f.events)
	}
	if slices.Contains(f.runArgs[0], "-challenge-set-aside") {
		t.Errorf("the replay run is given -challenge-set-aside: %v", f.runArgs[0])
	}
}
