package requestdriver_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
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
