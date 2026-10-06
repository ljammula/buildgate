package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
	"buildgate/internal/sessionconfig"
)

// TestDoctorCheckFactoryYMLReportsMissingAndPresent proves the new
// advisory check reports a warning naming .factory.yml when absent, and a
// clean pass once one exists.
func TestDoctorCheckFactoryYMLReportsMissingAndPresent(t *testing.T) {
	dir := t.TempDir()

	check := doctorCheckFactoryYML(dir)
	if check.Err == nil {
		t.Fatal("want a non-nil Err for a missing .factory.yml")
	}
	if !check.Advisory {
		t.Error("want Advisory=true -- a missing .factory.yml must never fail doctor outright")
	}
	if check.Fix == "" {
		t.Error("want a non-empty Fix")
	}

	if err := os.WriteFile(filepath.Join(dir, projectconfig.FileName), []byte("preflight_profile: brownfield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check = doctorCheckFactoryYML(dir)
	if check.Err != nil {
		t.Errorf("want a clean pass once %s exists, got Err=%v", projectconfig.FileName, check.Err)
	}
}

// TestDoctorApplyFixesWritesFactoryYMLWithYes proves -fix -yes writes a
// minimal .factory.yml without prompting.
func TestDoctorApplyFixesWritesFactoryYMLWithYes(t *testing.T) {
	// doctorApplyFixes always also evaluates the session config's own
	// release_* keys regardless of which fix a given test is really
	// about -- isolate HOME/XDG_CONFIG_HOME so that read (sessionconfig.
	// LoadDefault) never touches this machine's own real
	// ~/.config/factoryd/config.yml, matching every other doctorApplyFixes
	// test in this file.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	dir := t.TempDir()
	// go.mod makes detectVerifyCommand resolve "go test ./..." so the
	// written file's verify_command line can be asserted on.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	doctorApplyFixes(nil, dir, t.TempDir(), false, true, strings.NewReader(""), &out, "")

	path := filepath.Join(dir, projectconfig.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s was not written: %v", path, err)
	}
	if !strings.Contains(string(data), `verify_command: "go test ./..."`) {
		t.Errorf("%s = %q, want it to name the detected verify_command", path, data)
	}
	if !strings.Contains(out.String(), "wrote "+path) {
		t.Errorf("output = %q, want a line confirming the write", out.String())
	}
}

// TestDoctorApplyFixesDoesNotOverwriteExistingFactoryYML proves a
// pre-existing .factory.yml is left completely untouched.
func TestDoctorApplyFixesDoesNotOverwriteExistingFactoryYML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	dir := t.TempDir()
	path := filepath.Join(dir, projectconfig.FileName)
	original := "verify_command: \"custom\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	doctorApplyFixes(nil, dir, t.TempDir(), false, true, strings.NewReader(""), &out, "")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("%s was modified: got %q, want unchanged %q", path, data, original)
	}
}

// TestDoctorApplyFixesNonInteractiveWithoutYesNeverApplies proves a
// non-interactive invocation (no real TTY) with -fix but no -yes never
// applies anything, only reports what it would do -- the "never silently"
// requirement.
func TestDoctorApplyFixesNonInteractiveWithoutYesNeverApplies(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	doctorApplyFixes(nil, dir, t.TempDir(), false, false, strings.NewReader(""), &out, "")

	if _, err := os.Stat(filepath.Join(dir, projectconfig.FileName)); err == nil {
		t.Fatal(".factory.yml was written despite no -yes and no TTY -- must never apply silently")
	}
	if !strings.Contains(out.String(), "would apply") {
		t.Errorf("output = %q, want a line explaining what would be applied", out.String())
	}
	if !strings.Contains(out.String(), "-yes") {
		t.Errorf("output = %q, want it to mention -yes as the way to apply non-interactively", out.String())
	}
}

// TestDoctorApplyFixesBackfillsReleaseDefaultsWithYes proves that a
// session config missing release_* keys gets them appended, reusing
// quickstart's own backfill logic, with -fix -yes.
func TestDoctorApplyFixesBackfillsReleaseDefaultsWithYes(t *testing.T) {
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

	var out bytes.Buffer
	doctorApplyFixes(nil, "", t.TempDir(), false, true, strings.NewReader(""), &out, "")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"release_max_files_changed:", "release_max_insertions:", "release_rollback_plan:"} {
		if !strings.Contains(string(data), key) {
			t.Errorf("%s missing %q after -fix -yes:\n%s", cfgPath, key, data)
		}
	}
	// The original line must survive untouched (append-only).
	if !strings.Contains(string(data), "sandbox_image: img@sha256:") {
		t.Errorf("%s lost its original content:\n%s", cfgPath, data)
	}
}

// TestDataDirNeedsRepointingRequiresFailingCheckAndOutsideDataRoot proves the
// gate for repointing data_dir: both a failing -data-dir mount-visibility
// check AND a dataDir outside ~/buildgate (sessionconfig.DataRoot) are required.
func TestDataDirNeedsRepointingRequiresFailingCheckAndOutsideDataRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dockerReachable := doctorCheck{Name: "docker daemon reachable"}
	dockerDown := doctorCheck{Name: "docker daemon reachable", Err: os.ErrNotExist}
	mountFailing := doctorCheck{Name: "mount visibility (-data-dir reachable inside a container)", Err: os.ErrNotExist}
	mountPassing := doctorCheck{Name: "mount visibility (-data-dir reachable inside a container)"}

	failingCheck := []doctorCheck{mountFailing, dockerReachable}
	passingCheck := []doctorCheck{mountPassing, dockerReachable}

	outsideHome := filepath.Join(t.TempDir(), "elsewhere")
	insideHome := filepath.Join(home, "buildgate", "data")

	if !dataDirNeedsRepointing(failingCheck, outsideHome, false) {
		t.Error("want true: failing check + outside ~/buildgate + docker reachable + dataDir not explicit")
	}
	if !dataDirNeedsRepointing(failingCheck, filepath.Join(home, ".cache", "data"), false) {
		t.Error("want true: under $HOME but outside ~/buildgate, which colima does not share")
	}
	if dataDirNeedsRepointing(passingCheck, outsideHome, false) {
		t.Error("want false: check did not actually fail")
	}
	if dataDirNeedsRepointing(failingCheck, insideHome, false) {
		t.Error("want false: dataDir is already under ~/buildgate")
	}
	if dataDirNeedsRepointing(nil, outsideHome, false) {
		t.Error("want false: no data-dir mount-visibility check present at all")
	}
	// An adversarial review flagged that a mount failure caused by Docker
	// simply being down must never be misdiagnosed as "wrong directory".
	if dataDirNeedsRepointing([]doctorCheck{mountFailing, dockerDown}, outsideHome, false) {
		t.Error("want false: docker itself was not reachable in this doctor run")
	}
	if dataDirNeedsRepointing([]doctorCheck{mountFailing}, outsideHome, false) {
		t.Error("want false: no docker-reachable check present at all")
	}
	// The same adversarial review also flagged that an operator's own
	// explicit -data-dir flag must never be silently overridden by this fix.
	if dataDirNeedsRepointing(failingCheck, outsideHome, true) {
		t.Error("want false: -data-dir was given explicitly")
	}
}

// TestDoctorApplyFixesRepointsDataDirWithYes proves the data_dir repoint
// end to end: given a failing -data-dir mount-visibility check for a
// directory outside $HOME, -fix -yes rewrites data_dir in the session
// config and never moves the existing directory itself.
func TestDoctorApplyFixesRepointsDataDirWithYes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "xdg", "factoryd")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	oldDataDir := filepath.Join(t.TempDir(), "outside-home-data")
	if err := os.MkdirAll(oldDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+oldDataDir+"\nsandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	checks := []doctorCheck{
		{Name: "mount visibility (-data-dir reachable inside a container)", Err: os.ErrNotExist},
		{Name: "docker daemon reachable"},
	}

	var out bytes.Buffer
	doctorApplyFixes(checks, "", oldDataDir, false, true, strings.NewReader(""), &out, "")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	wantDataDir := filepath.Join(home, "buildgate", "data")
	if !strings.Contains(string(data), "data_dir: "+wantDataDir) {
		t.Errorf("%s = %q, want it to now set data_dir: %s", cfgPath, data, wantDataDir)
	}
	if strings.Count(string(data), "data_dir:") != 1 {
		t.Errorf("%s has more than one data_dir: line:\n%s", cfgPath, data)
	}
	if !strings.Contains(string(data), "sandbox_image: img@sha256:") {
		t.Errorf("%s lost its other content:\n%s", cfgPath, data)
	}
	if _, err := os.Stat(oldDataDir); err != nil {
		t.Errorf("old data dir %s must be left in place, not moved: %v", oldDataDir, err)
	}
	if !strings.Contains(out.String(), "mv "+oldDataDir+" "+wantDataDir) {
		t.Errorf("output = %q, want it to print the mv command for the operator to run themselves", out.String())
	}
}

// TestDoctorRepointDataDirOnlyReplacesTopLevelKey is the regression test
// for an adversarial-review finding: an indented `data_dir:`-looking line
// (nested inside some other mapping/block scalar) must NEVER be rewritten
// -- only a true top-level key at column 0. Before that fix (TrimLeft-based
// matching), this indented line would have been incorrectly replaced; this
// test fails against that old behavior and passes only once the
// column-0-only match is in place.
func TestDoctorRepointDataDirOnlyReplacesTopLevelKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	// models.m.extra_json, not a fictitious "some_block:": a real,
	// arbitrary-map-typed field nested under a real top-level mapping
	// key (a later adversarial review, checking that a re-load-as-
	// backstop this fixture must still parse under -- sessionconfig.Load
	// uses KnownFields(true), so an invented top-level key would fail
	// that reload regardless of doctorRepointDataDir's own correctness).
	original := "models:\n  m:\n    id: some-model\n    extra_json:\n      data_dir: nested-value-must-survive\nsandbox_image: img\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := sessionConfigForTest()

	if err := doctorRepointDataDir(cfgPath, &cfg, "/new/data"); err != nil {
		t.Fatalf("doctorRepointDataDir: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "      data_dir: nested-value-must-survive\n") {
		t.Errorf("indented data_dir: line was modified, want it untouched:\n%s", got)
	}
	if !strings.Contains(got, "\ndata_dir: /new/data\n") {
		t.Errorf("top-level data_dir: was not appended:\n%s", got)
	}
}

// TestDoctorRepointDataDirReplacesExistingTopLevelKeyOnly proves an
// EXISTING top-level data_dir: line is replaced in place (not duplicated)
// while an indented same-named line elsewhere is left alone.
func TestDoctorRepointDataDirReplacesExistingTopLevelKeyOnly(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	// models.m.extra_json, not a fictitious "some_block:" -- see
	// TestDoctorRepointDataDirOnlyReplacesTopLevelKey's own comment.
	original := "data_dir: /old/data\nmodels:\n  m:\n    id: some-model\n    extra_json:\n      data_dir: nested-value-must-survive\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := sessionConfigForTest()

	if err := doctorRepointDataDir(cfgPath, &cfg, "/new/data"); err != nil {
		t.Fatalf("doctorRepointDataDir: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Count(got, "data_dir:") != 2 {
		t.Fatalf("want exactly 2 data_dir: occurrences (1 top-level replaced + 1 nested untouched), got:\n%s", got)
	}
	if !strings.HasPrefix(got, "data_dir: /new/data\n") {
		t.Errorf("top-level data_dir: was not replaced with the new value:\n%s", got)
	}
	if !strings.Contains(got, "      data_dir: nested-value-must-survive\n") {
		t.Errorf("nested data_dir: line was modified:\n%s", got)
	}
	if strings.Contains(got, "/old/data") {
		t.Errorf("old top-level value still present:\n%s", got)
	}
}

// TestDoctorRepointDataDirQuotesSpecialValue proves the written value goes
// through YAML-safe quoting (quickstartYAMLScalar) rather than a raw,
// unquoted path -- a value containing a colon must round-trip as a plain
// string, not something YAML would otherwise misparse.
func TestDoctorRepointDataDirQuotesSpecialValue(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("sandbox_image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := sessionConfigForTest()
	trickyValue := "/data: with a colon"

	if err := doctorRepointDataDir(cfgPath, &cfg, trickyValue); err != nil {
		t.Fatalf("doctorRepointDataDir: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := sessionconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("reloaded config does not even parse as valid YAML: %v\ncontent:\n%s", err, data)
	}
	if reloaded.DataDir == nil || *reloaded.DataDir != trickyValue {
		t.Errorf("round-tripped data_dir = %v, want %q\ncontent:\n%s", reloaded.DataDir, trickyValue, data)
	}
}

// TestDoctorRepointDataDirMatchesQuotedAndSpacedKeyForms is the
// regression test for another adversarial-review finding: the original
// strings.HasPrefix(line, "data_dir:") match only ever recognized the
// bare, no-space form. A pre-existing
// `"data_dir": ""` or `data_dir : ""` line -- both valid YAML -- was
// invisible to it, so doctorRepointDataDir appended a SECOND, duplicate
// data_dir key instead of replacing the existing one, which
// gopkg.in/yaml.v3 then refuses to parse at all.
func TestDoctorRepointDataDirMatchesQuotedAndSpacedKeyForms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string
	}{
		{"double-quoted key, empty value", `"data_dir": ""` + "\n"},
		{"single-quoted key, empty value", `'data_dir': ''` + "\n"},
		{"space before colon", "data_dir : \"\"\n"},
		{"double-quoted key, real value, space before colon", `"data_dir"  :  "/old/data"` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yml")
			original := tc.existing + "sandbox_image: img\n"
			if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := sessionConfigForTest()

			if err := doctorRepointDataDir(cfgPath, &cfg, "/new/data"); err != nil {
				t.Fatalf("doctorRepointDataDir: %v", err)
			}

			data, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			if n := strings.Count(got, "data_dir"); n != 1 {
				t.Fatalf("%q occurs %d times, want exactly 1 (the quoted/spaced key must be REPLACED, not left alone with a second bare key appended):\n%s", "data_dir", n, got)
			}
			if !strings.Contains(got, "data_dir: /new/data\n") {
				t.Errorf("new value not written:\n%s", got)
			}
			reloaded, err := sessionconfig.Load(cfgPath)
			if err != nil {
				t.Fatalf("reloaded config does not parse (a duplicate key would fail exactly this way): %v\ncontent:\n%s", err, got)
			}
			if reloaded.DataDir == nil || *reloaded.DataDir != "/new/data" {
				t.Errorf("reloaded DataDir = %v, want /new/data", reloaded.DataDir)
			}
		})
	}
}

// TestDoctorRepointDataDirRestoresOriginalOnUnloadableResult is the
// regression test for the second half of that same finding: if the
// write somehow still produces a config sessionconfig.Load can't parse -- the backstop
// for a form doctorDataDirKeyPattern itself might still miss -- the
// original bytes must be restored rather than left corrupted, and the
// error must say so.
func TestDoctorRepointDataDirRestoresOriginalOnUnloadableResult(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	// Already invalid YAML (unclosed flow mapping) -- doctorRepointDataDir
	// will append a data_dir: line (no top-level match), but the RESULT
	// still can't parse, because the file was already broken.
	original := "sandbox_image: [img\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := sessionConfigForTest()

	err := doctorRepointDataDir(cfgPath, &cfg, "/new/data")
	if err == nil {
		t.Fatal("doctorRepointDataDir = nil error, want it to refuse an unloadable result")
	}
	if !strings.Contains(err.Error(), "unloadable") {
		t.Errorf("err = %v, want it to say the result was unloadable", err)
	}

	data, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Errorf("file = %q, want the original bytes restored:\n%q", data, original)
	}
}

// TestDoctorApplyFixesRefusesSymlinkedFactoryYML is the regression test
// for an adversarial-review finding: a symlink at .factory.yml's own path
// must never be followed/overwritten by the "write a minimal one" fix.
// Before that fix (Stat + plain os.WriteFile), a symlink there would have
// been silently followed and its target overwritten; this test fails
// against that old behavior (the fix would "succeed" and write through
// the symlink) and passes only once the Lstat+O_EXCL+O_NOFOLLOW create
// is in place.
func TestDoctorApplyFixesRefusesSymlinkedFactoryYML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-target.yml")
	if err := os.WriteFile(outside, []byte("do-not-touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, projectconfig.FileName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	var out bytes.Buffer
	doctorApplyFixes(nil, dir, t.TempDir(), false, true, strings.NewReader(""), &out, "")

	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do-not-touch\n" {
		t.Fatalf("symlink target was modified: %q", data)
	}
}

func sessionConfigForTest() sessionconfig.Config {
	return sessionconfig.Config{}
}
