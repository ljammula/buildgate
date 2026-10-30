package release

import (
	"reflect"
	"testing"
)

// The .buildgate/ directory is reserved to the factory: a change under it is
// refused whatever its name, in any case.
func TestProtectedFilesTouchedAlwaysProtectsBuildgateDir(t *testing.T) {
	got := MergePolicy{}.ProtectedFilesTouched([]string{".buildgate/index.json", ".buildgate/x", "main.go"})
	if !reflect.DeepEqual(got, []string{".buildgate/index.json", ".buildgate/x"}) {
		t.Fatalf("got %v", got)
	}
	r := cleanRun()
	r.ChangedFiles = []string{".buildgate/notes.txt"}
	assertMergePolicyReason(t, r, cleanPolicy(), ".buildgate/notes.txt")
}

func TestBuildgateDirProtectionFoldsCase(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, ".BuildGate/index.json", ".BUILDGATE/x")
	assertMergePolicyReason(t, r, cleanPolicy(), ".BuildGate/index.json")
}
