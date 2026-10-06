package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeFactoryd writes an executable shell script at dir/factoryd that
// prints "factoryd version <version>" when invoked with "version" -- a
// stand-in for a real factoryd binary, so factoryVersionOf's `<path>
// version` subprocess has something real to run against without building
// an actual binary per test case.
func writeFakeFactoryd(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "factoryd")
	script := "#!/bin/sh\necho factoryd version " + version + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake factoryd: %v", err)
	}
	return path
}

// TestDoctorWarnsOnShadowingBinary proves the done criterion for
// path-shadowing detection: a fixture PATH containing a second
// "factoryd"-named executable, distinct from the one doctorCheckPathShadowing
// is told is "self", produces an advisory warning naming both paths and
// self's own reported version. It deliberately does NOT assert the shadow's
// reported version -- see TestDoctorPathShadowingNeverExecutesShadowBinary:
// found via Codex review of PR #172 (P1), executing an untrusted $PATH
// entry to ask its version is itself a code-execution risk, so the shadow
// candidate is described via os.Stat (size/mtime) only, never run.
func TestDoctorWarnsOnShadowingBinary(t *testing.T) {
	selfDir := t.TempDir()
	shadowDir := t.TempDir()
	selfPath := writeFakeFactoryd(t, selfDir, "1.2.3-self")
	shadowPath := writeFakeFactoryd(t, shadowDir, "0.9.0-shadow")

	pathEnv := strings.Join([]string{shadowDir, selfDir}, string(os.PathListSeparator))

	check := doctorCheckPathShadowing(selfPath, pathEnv, false)

	if check.Err == nil {
		t.Fatalf("expected a warning, got nil Err")
	}
	if !check.Advisory {
		t.Errorf("expected Advisory: true (a shadowing binary should warn, not fail), got false")
	}
	msg := check.Err.Error()
	for _, want := range []string{selfPath, shadowPath, "1.2.3-self"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q missing %q", msg, want)
		}
	}
}

// TestDoctorPathShadowingNeverExecutesShadowBinary is the direct
// regression for the Codex P1 finding on PR #172: a shadowing $PATH entry
// must never be run, since it can be a stale build, a hung process, or a
// binary an attacker placed in an untrusted directory ahead of the real
// one on $PATH. The fixture shadow script writes a sentinel file when
// executed; the check must never trigger it.
func TestDoctorPathShadowingNeverExecutesShadowBinary(t *testing.T) {
	selfDir := t.TempDir()
	shadowDir := t.TempDir()
	selfPath := writeFakeFactoryd(t, selfDir, "1.2.3-self")

	sentinel := filepath.Join(shadowDir, "executed.sentinel")
	shadowPath := filepath.Join(shadowDir, "factoryd")
	script := "#!/bin/sh\ntouch " + sentinel + "\necho factoryd version 0.9.0-shadow\n"
	if err := os.WriteFile(shadowPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write sentinel shadow factoryd: %v", err)
	}

	pathEnv := strings.Join([]string{shadowDir, selfDir}, string(os.PathListSeparator))
	if check := doctorCheckPathShadowing(selfPath, pathEnv, false); check.Err == nil {
		t.Fatalf("expected a warning, got nil Err")
	}

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("shadow binary was executed (sentinel file created) -- doctor must never run an untrusted $PATH candidate")
	}
}

// TestDoctorNoWarningWithoutShadowingBinary confirms the check stays quiet
// (nil Err) when $PATH holds only the running binary itself, so a normal
// single-install setup never sees a spurious warning.
func TestDoctorNoWarningWithoutShadowingBinary(t *testing.T) {
	selfDir := t.TempDir()
	selfPath := writeFakeFactoryd(t, selfDir, "1.2.3-self")

	check := doctorCheckPathShadowing(selfPath, selfDir, false)

	if check.Err != nil {
		t.Fatalf("expected no warning, got: %v", check.Err)
	}
}

// TestDoctorPathShadowingSafeDirRunsVersionAndPrintsFix proves a shadow
// found in one of doctorPathShadowSafeDir's trusted locations ($HOME/.local
// /bin here) gets its own "version" output run directly (unlike the
// untrusted-directory case above) and a concrete rm/mv fix line, without
// -fix env var applying anything.
func TestDoctorPathShadowingSafeDirRunsVersionAndPrintsFix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	selfDir := t.TempDir()
	selfPath := writeFakeFactoryd(t, selfDir, "1.2.3-self")

	safeDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(safeDir, 0o755); err != nil {
		t.Fatalf("mkdir safe dir: %v", err)
	}
	shadowPath := writeFakeFactoryd(t, safeDir, "0.9.0-shadow")

	pathEnv := strings.Join([]string{safeDir, selfDir}, string(os.PathListSeparator))
	check := doctorCheckPathShadowing(selfPath, pathEnv, true)

	if check.Err == nil {
		t.Fatalf("expected a warning, got nil Err")
	}
	msg := check.Err.Error()
	if strings.Contains(msg, "0.9.0-shadow") {
		t.Errorf("warning %q executed the shadow's version subcommand; a shadow is never executed, trusted directory or not", msg)
	}
	if !strings.Contains(check.Fix, "rm "+shadowPath) {
		t.Errorf("Fix %q missing an rm command for the safe-dir shadow", check.Fix)
	}
	if _, err := os.Stat(shadowPath); err != nil {
		t.Fatalf("shadow binary should still exist (no FACTORYD_DOCTOR_APPLY_PATH_FIX): %v", err)
	}
}

// TestDoctorPathShadowingAppliesFixWhenEnvVarSet proves that with -fix AND
// FACTORYD_DOCTOR_APPLY_PATH_FIX=1, a safe-dir shadow is renamed to
// "<path>.stale-<date>" rather than deleted -- opt-in since this touches
// the operator's own filesystem.
func TestDoctorPathShadowingAppliesFixWhenEnvVarSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(doctorApplyPathFixEnvVar, "1")
	selfDir := t.TempDir()
	selfPath := writeFakeFactoryd(t, selfDir, "1.2.3-self")

	safeDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(safeDir, 0o755); err != nil {
		t.Fatalf("mkdir safe dir: %v", err)
	}
	shadowPath := writeFakeFactoryd(t, safeDir, "0.9.0-shadow")

	pathEnv := strings.Join([]string{safeDir, selfDir}, string(os.PathListSeparator))
	check := doctorCheckPathShadowing(selfPath, pathEnv, true)

	if check.Err == nil {
		t.Fatalf("expected a warning, got nil Err")
	}
	if _, err := os.Stat(shadowPath); err == nil {
		t.Fatalf("shadow binary should have been renamed away, still present at %s", shadowPath)
	}
	matches, err := filepath.Glob(shadowPath + ".stale-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one renamed stale file, got %v", matches)
	}
}

// TestDoctorPathShadowingWithoutFixNeverTouchesSafeDirBinary proves that
// without -fix (fix=false), a safe-dir shadow is only ever described, never
// renamed or deleted, even with the env var set -- the -fix flag itself
// gates any filesystem side effect.
func TestDoctorPathShadowingWithoutFixNeverTouchesSafeDirBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(doctorApplyPathFixEnvVar, "1")
	selfDir := t.TempDir()
	selfPath := writeFakeFactoryd(t, selfDir, "1.2.3-self")

	safeDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(safeDir, 0o755); err != nil {
		t.Fatalf("mkdir safe dir: %v", err)
	}
	shadowPath := writeFakeFactoryd(t, safeDir, "0.9.0-shadow")

	pathEnv := strings.Join([]string{safeDir, selfDir}, string(os.PathListSeparator))
	if check := doctorCheckPathShadowing(selfPath, pathEnv, false); check.Err == nil {
		t.Fatalf("expected a warning, got nil Err")
	}
	if _, err := os.Stat(shadowPath); err != nil {
		t.Fatalf("shadow binary should still exist without -fix: %v", err)
	}
}

// TestDoctorPathShadowSafeDir covers doctorPathShadowSafeDir's own
// directory allowlist directly.
func TestDoctorPathShadowSafeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GOPATH", "")
	t.Setenv("HOMEBREW_PREFIX", "")

	cases := []struct {
		dir  string
		want bool
	}{
		{filepath.Join(home, ".local", "bin"), true},
		{filepath.Join(home, "go", "bin"), true},
		{"/usr/local/bin", true},
		{"/opt/homebrew/bin", true},
		{"/tmp/some-random-dir", false},
		{filepath.Join(home, "Downloads"), false},
	}
	for _, c := range cases {
		if got := doctorPathShadowSafeDir(c.dir); got != c.want {
			t.Errorf("doctorPathShadowSafeDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}
