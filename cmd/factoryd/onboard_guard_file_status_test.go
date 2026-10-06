package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOnboardGuardFileStatusReportsExistingFiles covers onboardGuardFileStatus
// directly: a future caller (factoryd quickstart) needs to trust this cheap
// existence check before ever calling onboard, so it's exercised here on its
// own rather than only indirectly through onboardMain's refusal path.
func TestOnboardGuardFileStatusReportsExistingFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	existing, err := onboardGuardFileStatus(root, false)
	if err != nil {
		t.Fatalf("onboardGuardFileStatus on empty root: %v", err)
	}
	if len(existing) != 0 {
		t.Fatalf("empty root: got existing = %v, want none", existing)
	}

	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	specMD := filepath.Join(root, "spec", "spec.md")
	if err := os.WriteFile(specMD, []byte("# spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	architectureMD := filepath.Join(root, "ARCHITECTURE.md")
	if err := os.WriteFile(architectureMD, []byte("# architecture"), 0o644); err != nil {
		t.Fatal(err)
	}

	existing, err = onboardGuardFileStatus(root, false)
	if err != nil {
		t.Fatalf("onboardGuardFileStatus with two files present: %v", err)
	}
	want := map[string]bool{specMD: true, architectureMD: true}
	if len(existing) != len(want) {
		t.Fatalf("got existing = %v, want exactly %v", existing, want)
	}
	for _, p := range existing {
		if !want[p] {
			t.Errorf("unexpected path reported as existing: %s", p)
		}
	}
}

// TestOnboardGuardFileStatusFactoryYmlOnlyWhenRequested confirms
// includeFactoryYml gates the .factory.yml check, per onboardGuardFileStatus's
// own doc comment: unconditionally checking it would report a false
// "already scaffolded" for a repo with a .factory.yml from an earlier
// plain-onboard run that never passed -write-factory-yml.
func TestOnboardGuardFileStatusFactoryYmlOnlyWhenRequested(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	factoryYML := filepath.Join(root, ".factory.yml")
	if err := os.WriteFile(factoryYML, []byte("verify_command: make verify\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	existing, err := onboardGuardFileStatus(root, false)
	if err != nil {
		t.Fatalf("onboardGuardFileStatus(includeFactoryYml=false): %v", err)
	}
	if len(existing) != 0 {
		t.Fatalf("includeFactoryYml=false: got existing = %v, want none", existing)
	}

	existing, err = onboardGuardFileStatus(root, true)
	if err != nil {
		t.Fatalf("onboardGuardFileStatus(includeFactoryYml=true): %v", err)
	}
	if len(existing) != 1 || existing[0] != factoryYML {
		t.Fatalf("includeFactoryYml=true: got existing = %v, want [%s]", existing, factoryYML)
	}
}

// TestOnboardGuardFileStatusDanglingSymlinkCountsAsExisting matches
// writeScaffoldFiles' own Lstat-based symlink handling: a dangling symlink
// at a guard path still counts as "exists" here, since onboard's real
// refusal treats it the same way.
func TestOnboardGuardFileStatusDanglingSymlinkCountsAsExisting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	architectureMD := filepath.Join(root, "ARCHITECTURE.md")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), architectureMD); err != nil {
		t.Fatal(err)
	}

	existing, err := onboardGuardFileStatus(root, false)
	if err != nil {
		t.Fatalf("onboardGuardFileStatus with dangling symlink: %v", err)
	}
	if len(existing) != 1 || existing[0] != architectureMD {
		t.Fatalf("got existing = %v, want [%s]", existing, architectureMD)
	}
}
