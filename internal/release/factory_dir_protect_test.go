package release

import (
	"reflect"
	"testing"

	"buildgate/internal/projectconfig"
)

var factoryDirProtected = []string{
	".factory/lint.sh", ".factory/a/b/c.sh", ".Factory/x.sh", ".FACTORY/a/b", ".fAcToRy/y", ".factory", ".Factory", ".FACTORY",
}

var factoryDirNotProtected = []string{
	".factoryx/a", ".factory-x/a", ".factory.yaml", "factory/a", "src/.factory/a", "a/.Factory/x.sh", "x.factory", ".factor/a",
}

func TestFactoryDirNameIsDefinedOnceInProjectConfig(t *testing.T) {
	if projectconfig.DirName != ".factory" {
		t.Fatalf("DirName = %q", projectconfig.DirName)
	}
}

func TestProtectedFilesTouchedProtectsTheFactoryDirInAnyCase(t *testing.T) {
	changed := append(append([]string{"main.go"}, factoryDirProtected...), factoryDirNotProtected...)
	got := MergePolicy{}.ProtectedFilesTouched(changed)
	if !reflect.DeepEqual(got, factoryDirProtected) {
		t.Fatalf("got %v, want %v", got, factoryDirProtected)
	}
	// An operator's own list changes nothing about it.
	got = MergePolicy{ProtectedPaths: []string{"cfg/x"}}.ProtectedFilesTouched([]string{"cfg/x", ".Factory/a", "src/.factory/a"})
	if !reflect.DeepEqual(got, []string{"cfg/x", ".Factory/a"}) {
		t.Fatalf("got %v", got)
	}
}

func TestProtectedFilesTouchedByRunProtectsTheFactoryDirInAnyCase(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(append([]string{"main.go"}, factoryDirProtected...), factoryDirNotProtected...)
	got := MergePolicy{}.ProtectedFilesTouchedByRun(r)
	if !reflect.DeepEqual(got, factoryDirProtected) {
		t.Fatalf("got %v, want %v", got, factoryDirProtected)
	}
}

func TestProtectedFilesTouchedByRunNeverExemptsTheFactoryDir(t *testing.T) {
	for _, p := range []string{".factory/lint.sh", ".Factory/lint.sh", ".FACTORY/a/b"} {
		r := cleanRun()
		r.ChangedFiles = []string{p}
		if got := (MergePolicy{}).ProtectedFilesTouchedByRun(r); !reflect.DeepEqual(got, []string{p}) {
			t.Errorf("%s: got %v, want it refused", p, got)
		}
		assertMergePolicyReason(t, r, cleanPolicy(), p)
	}
}

func TestMergePolicyCheckDeniesAChangeUnderTheFactoryDir(t *testing.T) {
	for _, p := range []string{".factory/lint.sh", ".Factory/lint.sh", ".FACTORY/lint.sh", ".factory"} {
		r := cleanRun()
		r.ChangedFiles = append(r.ChangedFiles, p)
		cfg := cleanPolicy()
		cfg.ProtectedPaths = nil
		assertMergePolicyReason(t, r, cfg, p)
	}
	for _, p := range factoryDirNotProtected {
		r := cleanRun()
		r.ChangedFiles = append(r.ChangedFiles, p)
		cfg := cleanPolicy()
		cfg.ProtectedPaths = nil
		if passed, reasons := MergePolicyCheck(r, cfg); !passed {
			t.Errorf("%s denied: %v", p, reasons)
		}
	}
}
