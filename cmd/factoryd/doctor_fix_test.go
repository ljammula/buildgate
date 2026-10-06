package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDoctorMakefile writes a Makefile at dir with (or without) the
// three image targets -fix dispatches to, for resolveDoctorRepoRoot.
func writeDoctorMakefile(t *testing.T, dir string, withTargets bool) {
	t.Helper()
	content := "all:\n\t@true\n"
	if withTargets {
		for _, target := range doctorImageMakeTargets {
			content += target + ": .local-registry\n\t@true\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(content), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
}

func TestResolveDoctorRepoRootPrefersFlagThenEnv(t *testing.T) {
	t.Parallel()
	if got, err := resolveDoctorRepoRoot("/from/flag", "/from/env", "", ""); err != nil || got != "/from/flag" {
		t.Errorf("flag+env: got %q, %v; want /from/flag", got, err)
	}
	if got, err := resolveDoctorRepoRoot("", "/from/env", "", ""); err != nil || got != "/from/env" {
		t.Errorf("env only: got %q, %v; want /from/env", got, err)
	}
}

// TestResolveDoctorRepoRootPrefersImageSourceRootOverExecutableHeuristic
// is the regression test for a real finding (adversarial review of the
// ghcr-removal change): after a normal `go install` (to e.g. ~/go/bin,
// nowhere near any checkout), the executable heuristic always found
// nothing, so quickstart's own missing/stale-image rebuild prompt never
// appeared on the single most common post-`make install` machine setup.
// imageSourceRoot -- the session config's own recorded image_source_root
// -- must be checked before that heuristic, and must win over it even
// when the executable heuristic WOULD have found something (an unrelated
// checkout the binary happens to sit under is never more authoritative
// than the checkout images were actually recorded against).
func TestResolveDoctorRepoRootPrefersImageSourceRootOverExecutableHeuristic(t *testing.T) {
	t.Parallel()
	if got, err := resolveDoctorRepoRoot("", "", "/from/image-source-root", ""); err != nil || got != "/from/image-source-root" {
		t.Errorf("imageSourceRoot only: got %q, %v; want /from/image-source-root", got, err)
	}
	root := t.TempDir()
	writeDoctorMakefile(t, root, true)
	if got, err := resolveDoctorRepoRoot("", "", "/from/image-source-root", filepath.Join(root, "bin", "factoryd")); err != nil || got != "/from/image-source-root" {
		t.Errorf("imageSourceRoot + a real executable heuristic match: got %q, %v; want the imageSourceRoot to win: /from/image-source-root", got, err)
	}
}

// TestResolveDoctorRepoRootFallsBackToTheBinarysParentOnlyWithTheTargets:
// with no flag or env, a binary at <checkout>/bin/factoryd resolves to
// <checkout> only when its Makefile defines the image targets; a
// Makefile lacking them, or none at all, errors naming -repo-root.
func TestResolveDoctorRepoRootFallsBackToTheBinarysParentOnlyWithTheTargets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		makefile    bool
		withTargets bool
		wantFound   bool
	}{
		{"makefile with targets", true, true, true},
		{"makefile without targets", true, false, false},
		{"no makefile", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.makefile {
				writeDoctorMakefile(t, root, tc.withTargets)
			}
			got, err := resolveDoctorRepoRoot("", "", "", filepath.Join(root, "bin", "factoryd"))
			if tc.wantFound {
				if err != nil || filepath.Clean(got) != root {
					t.Errorf("got %q, %v; want %q", got, err, root)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "-repo-root") {
				t.Errorf("got %q, %v; want an error naming -repo-root", got, err)
			}
		})
	}
	if _, err := resolveDoctorRepoRoot("", "", "", ""); err == nil {
		t.Error("unknown executable: want an error")
	}
}

// stubDoctorMakeImage replaces the -fix build seam for one test and
// records the (repoRoot, target) it was dispatched with.
func stubDoctorMakeImage(dp *deps, t *testing.T, stdout string, err error) (calls *[][2]string) {
	t.Helper()
	calls = new([][2]string)
	prev := fakeDockerOf(dp).makeImageFn
	fakeDockerOf(dp).makeImageFn = func(repoRoot, target string, vars ...string) (string, error) {
		*calls = append(*calls, [2]string{repoRoot, target})
		return stdout, err
	}
	t.Cleanup(func() { fakeDockerOf(dp).makeImageFn = prev })
	return calls
}

func writeAlwaysOKDocker(t *testing.T) string {
	t.Helper()
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return fakeDocker
}

var testDoctorProxyImage = doctorImage{label: "registry proxy image", flag: "-registry-proxy-image", image: "localhost:5050/factoryd-registry-proxy:missing", makeTarget: "registry-proxy-image"}

func TestDoctorFixBuildsTheImageAndReportsTheReferenceToUse(t *testing.T) {
	dp := newTestDeps(t)
	const ref = "localhost:5050/factoryd-registry-proxy@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	calls := stubDoctorMakeImage(dp, t, "docker push output\nlocal: digest: sha256:0123 size: 1\n"+ref+"\n", nil)
	check, built := doctorFixImageByBuilding(dp, writeAlwaysOKDocker(t), testDoctorProxyImage, "/repo")
	if check.Err != nil {
		t.Fatalf("Err = %v, want nil after a successful build", check.Err)
	}
	if built != ref {
		t.Errorf("built = %q, want %q", built, ref)
	}
	if want := "-registry-proxy-image " + ref; check.Use != want {
		t.Errorf("Use = %q, want %q", check.Use, want)
	}
	if want := [][2]string{{"/repo", "registry-proxy-image"}}; fmt.Sprint(*calls) != fmt.Sprint(want) {
		t.Errorf("make dispatched as %v, want %v", *calls, want)
	}
	var out strings.Builder
	runDoctorChecks([]doctorCheck{check}, &out)
	if !strings.Contains(out.String(), "      use: -registry-proxy-image "+ref) {
		t.Errorf("report lacks the use: line:\n%s", out.String())
	}
}

// TestRunDoctorChecksSummaryExcludesAdvisoriesFromPassedCount is the
// regression test for the P2 finding from Codex review of PR #142: an
// advisory ("warn") check must not be silently counted as "passed" in the
// summary line, or an operator reading only that line (not the individual
// check output above it) would never learn a warning was printed at all.
func TestRunDoctorChecksSummaryExcludesAdvisoriesFromPassedCount(t *testing.T) {
	checks := []doctorCheck{
		{Name: "ok check"},
		{Name: "warn check", Err: errors.New("advisory issue"), Advisory: true},
	}
	var out strings.Builder
	failed := runDoctorChecks(checks, &out)
	if failed != 0 {
		t.Errorf("failed = %d, want 0 -- an advisory must never fail the overall check", failed)
	}
	if want := "1/2 checks passed (1 warning(s))"; !strings.Contains(out.String(), want) {
		t.Errorf("summary = %q, want it to contain %q (the warn check must not count as passed)", out.String(), want)
	}
}

// TestResolveConfiguredDataDirAppliesSessionConfig is the regression test
// for the round-3 Codex finding on PR #142: -data-dir must resolve from
// the session config's own data_dir the same way worker's
// applySessionConfig does, when the flag wasn't explicit on this
// invocation's own command line.
func TestResolveConfiguredDataDirAppliesSessionConfig(t *testing.T) {
	defaultPath := isolateSessionConfig(t)
	writeSessionConfig(t, defaultPath, "data_dir: /configured/data\n")

	if got := resolveConfiguredDataDir(false, "data", ""); got != "/configured/data" {
		t.Errorf("resolveConfiguredDataDir(false, ...) = %q, want the session config's data_dir", got)
	}
	if got := resolveConfiguredDataDir(true, "data", ""); got != "data" {
		t.Errorf("resolveConfiguredDataDir(true, ...) = %q, want the explicit flag value unchanged (explicit always wins)", got)
	}
}

// TestResolveConfiguredDataDirDefaultsWithoutSessionConfig confirms no
// session config on disk leaves -data-dir's own default untouched.
func TestResolveConfiguredDataDirDefaultsWithoutSessionConfig(t *testing.T) {
	isolateSessionConfig(t)
	if got := resolveConfiguredDataDir(false, "data", ""); got != "data" {
		t.Errorf("resolveConfiguredDataDir(false, ...) = %q, want the unchanged default with no session config on disk", got)
	}
}

// TestDoctorRefreshInputsAfterFixClearsReleasePolicyWarning is the
// regression test proving that after doctorApplyFixes backfills missing
// release_* keys onto disk, doctorMain must re-resolve its doctorInputs
// from the now-changed config before re-running the check list --
// otherwise the same run's own final report still shows the "release
// policy allows a PR" warning it had just fixed. Exercises the
// exact sequence doctorMain runs: doctorApplyFixes, then
// doctorRefreshInputsAfterFix, then doctorCheckReleasePolicy on the
// refreshed doctorInputs.
func TestDoctorRefreshInputsAfterFixClearsReleasePolicyWarning(t *testing.T) {
	configPath := isolateSessionConfig(t)
	writeSessionConfig(t, configPath, "sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n")

	// Before the fix: doctorInputs built from the pre-fix (missing
	// release_*) settings warns, matching the config's actual state.
	before := doctorInputs{}
	check := doctorCheckReleasePolicy(before.releaseMaxFilesChanged, before.releaseMaxInsertions, before.releaseRollbackPlan)
	if check.Err == nil {
		t.Fatal("doctorCheckReleasePolicy = no error before the fix, want a warning (the config has no release_* keys yet)")
	}

	var out strings.Builder
	doctorApplyFixes(nil, "", t.TempDir(), false, true, strings.NewReader(""), &out, "")

	refreshed, _, err := doctorRefreshInputsAfterFix(before, false, "data", "")
	if err != nil {
		t.Fatalf("doctorRefreshInputsAfterFix: %v", err)
	}
	check = doctorCheckReleasePolicy(refreshed.releaseMaxFilesChanged, refreshed.releaseMaxInsertions, refreshed.releaseRollbackPlan)
	if check.Err != nil {
		t.Errorf("doctorCheckReleasePolicy after refresh = %v, want no error -- doctorApplyFixes already backfilled the config, so the refreshed doctorInputs must reflect that", check.Err)
	}
}

// TestDoctorApplyBuiltImageRefsCarriesFreshlyBuiltRefForward is the
// regression test for one symptom of an adversarial-review finding:
// doctorRunChecks' own doctorFixAbsentImages call substitutes a
// freshly built image ref into a LOCAL copy of doctorInputs that never
// reaches doctorMain's own `in` variable -- so a naive re-evaluation after
// -fix still names the unbuilt canonical ref. doctorApplyBuiltImageRefs
// must recover the built ref from the checks slice (via
// quickstartBuiltImageRefs' own "-sandbox-image <ref>"-shaped Use field)
// and apply it to `in`.
func TestDoctorApplyBuiltImageRefsCarriesFreshlyBuiltRefForward(t *testing.T) {
	checks := []doctorCheck{
		{Name: "sandbox image present (canonical@sha256:aaaa)"},
		{Name: "registry proxy image present (built-proxy@sha256:bbbb)", Use: "-registry-proxy-image built-proxy@sha256:bbbb"},
	}
	in := doctorInputs{sandboxImage: "canonical@sha256:aaaa", registryProxyImage: "canonical-proxy@sha256:aaaa"}
	got := doctorApplyBuiltImageRefs(in, checks)
	// sandboxImage has no Use field on its check (nothing was built for
	// it in this fixture) -- must stay unchanged.
	if got.sandboxImage != "canonical@sha256:aaaa" {
		t.Errorf("sandboxImage = %q, want unchanged %q (no build recorded)", got.sandboxImage, "canonical@sha256:aaaa")
	}
	if got.registryProxyImage != "built-proxy@sha256:bbbb" {
		t.Errorf("registryProxyImage = %q, want the freshly built ref from checks, not the stale canonical %q", got.registryProxyImage, in.registryProxyImage)
	}
}

// TestDoctorReplaceCheckSwapsByNameAndLeavesOthersUntouched is the
// regression test for the other symptom of that same adversarial-review
// finding: re-evaluating a check in place must never re-run or duplicate
// an unrelated check (in particular, an image-pull check) -- it replaces
// exactly the entry whose Name matches, and appends only when no such
// check already exists.
func TestDoctorReplaceCheckSwapsByNameAndLeavesOthersUntouched(t *testing.T) {
	checks := []doctorCheck{
		{Name: "sandbox image present (built@sha256:bbbb)"},
		{Name: "release policy allows a PR", Err: errors.New("stale warning")},
	}
	updated := doctorCheck{Name: "release policy allows a PR"}
	got := doctorReplaceCheck(checks, updated)
	if len(got) != 2 {
		t.Fatalf("len(checks) = %d, want 2 -- replacing an existing check must never grow the slice", len(got))
	}
	if got[0].Name != "sandbox image present (built@sha256:bbbb)" || got[0].Err != nil {
		t.Errorf("image-pull check = %+v, want it left completely untouched", got[0])
	}
	if got[1].Err != nil {
		t.Errorf("release policy check = %+v, want the stale warning replaced with the fresh (passing) result", got[1])
	}

	appended := doctorReplaceCheck(checks, doctorCheck{Name: "a check not yet present"})
	if len(appended) != 3 || appended[2].Name != "a check not yet present" {
		t.Errorf("appended = %+v, want a third entry for a Name with no existing match", appended)
	}
}

// TestDoctorFactoryYMLCheckClearsAfterFixWritesIt is the regression test
// for an adversarial-review finding: doctorMain's -fix re-evaluation
// left doctorCheckFactoryYML out entirely, so the same
// run still reported "no .factory.yml" right after doctorApplyFixes had
// just written one. Exercises the exact composition doctorMain now runs:
// doctorReplaceCheck(checks, doctorCheckFactoryYML(workspace)).
func TestDoctorFactoryYMLCheckClearsAfterFixWritesIt(t *testing.T) {
	workspace := t.TempDir()
	checks := []doctorCheck{
		doctorCheckFactoryYML(workspace), // the pre-fix, failing result
	}
	if checks[0].Err == nil {
		t.Fatal("doctorCheckFactoryYML = no error before the fix, want a warning (no .factory.yml written yet)")
	}

	// Simulates doctorApplyFixes' own item (a): write a minimal .factory.yml.
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	refreshed := doctorReplaceCheck(checks, doctorCheckFactoryYML(workspace))
	if len(refreshed) != 1 {
		t.Fatalf("len(refreshed) = %d, want 1 (replaced in place, not appended)", len(refreshed))
	}
	if refreshed[0].Err != nil {
		t.Errorf("doctorCheckFactoryYML after refresh = %v, want no error -- the fix already wrote .factory.yml", refreshed[0].Err)
	}
}

// TestDoctorReevaluateDataDirDependentChecksRefreshesMountVisibility is
// the first half of that same finding: when the sandbox image DOES
// resolve, both check families are actually re-run against the new
// data_dir (a nonexistent Docker binary makes the mount-visibility probe fail
// deterministically and quickly, the same fixture pattern
// TestWorkerConfigRefusesWhenDataDirIsNotMountVisible already uses,
// proving the check was re-probed rather than left at its stale,
// pre-repoint PASS).
func TestDoctorReevaluateDataDirDependentChecksRefreshesMountVisibility(t *testing.T) {
	in := doctorInputs{
		sandboxDocker: "factoryd-doctor-test-nonexistent-binary",
		workspace:     t.TempDir(),
		dataDir:       t.TempDir(),
	}
	checks := []doctorCheck{
		{Name: "mount visibility (-data-dir reachable inside a container)"}, // stale pass
	}
	got := doctorReevaluateDataDirDependentChecks(context.Background(), checks, in)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Err == nil {
		t.Error("mount visibility check has no error after re-evaluation, want it actually re-probed against the nonexistent Docker binary (a stale PASS left untouched would also show no error)")
	}
}

// TestDoctorApplyFixesReleaseCommentNamesDoctorNotQuickstart is the
// regression test proving the comment quickstartAppendReleaseDefaultsText
// appends must name whichever command actually wrote it -- before this
// fix it always said "factoryd quickstart" even when `factoryd doctor
// -fix` was the one appending it.
func TestDoctorApplyFixesReleaseCommentNamesDoctorNotQuickstart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "xdg", "factoryd")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	doctorApplyFixes(nil, "", t.TempDir(), false, true, strings.NewReader(""), &out, "")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# added by factoryd doctor -fix ") {
		t.Errorf("%s = %q, want the appended comment to name factoryd doctor -fix as the writer", cfgPath, data)
	}
	if strings.Contains(string(data), "# added by factoryd quickstart") {
		t.Errorf("%s = %q, must not claim factoryd quickstart wrote this -- factoryd doctor -fix did", cfgPath, data)
	}
}

// TestResolveConfiguredDataDirPrefersConfigPathOverDecoyDefault is
// `doctor`'s own half of the regression for "doctor has no -config flag
// at all": with a -config path given, doctor must resolve -data-dir from
// THAT file, never from whatever sits at the default search path -- the
// same "decoy config" scenario the live walk hit with `serve`.
func TestResolveConfiguredDataDirPrefersConfigPathOverDecoyDefault(t *testing.T) {
	defaultPath := isolateSessionConfig(t)
	writeSessionConfig(t, defaultPath, "data_dir: /decoy/data\n")

	namedPath := filepath.Join(t.TempDir(), "named-config.yml")
	writeSessionConfig(t, namedPath, "data_dir: /named/data\n")

	if got := resolveConfiguredDataDir(false, "data", namedPath); got != "/named/data" {
		t.Errorf("resolveConfiguredDataDir(false, _, namedPath) = %q, want the -config-named config's /named/data (got the default-path decoy instead)", got)
	}
}

func TestDoctorFixFailsWhenMakeFails(t *testing.T) {
	dp := newTestDeps(t)
	stubDoctorMakeImage(dp, t, "", errors.New("exit status 2"))
	check, built := doctorFixImageByBuilding(dp, writeAlwaysOKDocker(t), testDoctorProxyImage, "/repo")
	if check.Err == nil || !strings.Contains(check.Err.Error(), "make -C /repo registry-proxy-image") {
		t.Errorf("Err = %v, want the failed make command named", check.Err)
	}
	if built != "" || check.Use != "" {
		t.Errorf("built = %q, Use = %q, want both empty after a failed build", built, check.Use)
	}
}

func TestDoctorFixFailsWhenMakePrintsNoDigestPinnedReference(t *testing.T) {
	dp := newTestDeps(t)
	stubDoctorMakeImage(dp, t, "docker push output\nlocalhost:5050/factoryd-registry-proxy:local\n", nil)
	check, _ := doctorFixImageByBuilding(dp, writeAlwaysOKDocker(t), testDoctorProxyImage, "/repo")
	if check.Err == nil || !strings.Contains(check.Err.Error(), "did not print a digest-pinned reference") {
		t.Errorf("Err = %v, want the missing-digest diagnosis", check.Err)
	}
}

// TestIntegrationDoctorWithoutFixNamesTheFixFlag proves the FAIL line for
// an absent image now points at -fix and the Makefile target instead of
// leaving the operator to discover the make target on their own.
func TestIntegrationDoctorWithoutFixNamesTheFixFlag(t *testing.T) {
	t.Parallel()
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	// sandbox-docker is config-file only as of flags-consolidate
	// (2026-09-10; see sessionconfig.Settings) -- pointed at an isolated
	// session config (see isolatedSessionConfigEnv) rather than the real
	// developer one.
	cmd := factorydCommand(t, "doctor",
		"-sandbox-image", "localhost:5050/buildgate-worker:missing",
		"-registry-proxy-image", "localhost:5050/factoryd-registry-proxy:missing")
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeDocker+"\n")...)
	out, _ := cmd.CombinedOutput()
	for _, want := range []string{
		"rerun with -fix to build a local copy (make sandbox-image)",
		"rerun with -fix to build it locally (make registry-proxy-image)",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestDoctorFixAbsentImagesSkipsBuildsWhenDockerIsDown: with the daemon
// unreachable a build would fail the same way after wasting minutes, so
// -fix builds nothing and leaves the daemon check to report the cause.
func TestDoctorFixAbsentImagesSkipsBuildsWhenDockerIsDown(t *testing.T) {
	dp := newTestDeps(t)
	calls := 0
	orig := fakeDockerOf(dp).makeImageFn
	fakeDockerOf(dp).makeImageFn = func(repoRoot, target string, vars ...string) (string, error) { calls++; return "", nil }
	t.Cleanup(func() { fakeDockerOf(dp).makeImageFn = orig })
	in := doctorInputs{sandboxDocker: filepath.Join(t.TempDir(), "no-docker")}
	built, _ := doctorFixAbsentImages(dp, in, t.TempDir())
	if len(built) != 0 || calls != 0 {
		t.Fatalf("built = %+v, make calls = %d; want no build attempts with Docker down", built, calls)
	}
}
