package main

import (
	"path/filepath"
	"testing"
)

// TestDoctorRegisterNotifyTestFlagParses proves -notify-test is
// registered and parses correctly on the real doctor FlagSet -- not
// exercised via a direct doctorMain call: doctorMain's own preflight
// banner (factoryVersionOf) shells out to os.Executable(), which under
// `go test` is the compiled test binary itself, and invoking that
// binary as a subprocess with no -test.* flags re-enters this whole
// package's TestMain/runTests setup rather than doing anything
// version-related -- exactly the kind of slow, surprising recursion a
// unit test must not trigger. doctorNotifyTestMain itself (the actual
// new logic) is tested directly by
// TestDoctorNotifyTestMainNeverErrors below, with no dependency on
// doctorMain at all.
func TestDoctorRegisterNotifyTestFlagParses(t *testing.T) {
	t.Parallel()
	flags, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _ := newDoctorFlags()
	notifyTest := doctorRegisterNotifyTestFlag(flags)
	if err := flags.Parse([]string{"-notify-test"}); err != nil {
		t.Fatalf("flags.Parse(-notify-test): %v", err)
	}
	if !*notifyTest {
		t.Error("*notifyTest = false after -notify-test, want true")
	}
}

// TestDoctorNotifyTestMainNeverErrors mirrors DesktopNotifier's own
// documented contract (Notify never errors -- every failure mode is a
// silent no-op) at this command's own level, across every mechanism this
// test machine happens to have available.
func TestDoctorNotifyTestMainNeverErrors(t *testing.T) {
	t.Parallel()
	if err := doctorNotifyTestMain(t.TempDir()); err != nil {
		t.Fatalf("doctorNotifyTestMain: %v", err)
	}
}

// TestNotifyTestClickTargetIsTheConfiguredDataDirAsAnAbsolutePath: the
// click runs `open <target>` from terminal-notifier's working directory,
// where the unresolved -data-dir default ("data") names nothing, so a
// click on the test notification did nothing.
func TestNotifyTestClickTargetIsTheConfiguredDataDirAsAnAbsolutePath(t *testing.T) {
	defaultPath := isolateSessionConfig(t)
	writeSessionConfig(t, defaultPath, "data_dir: /configured/data\n")

	flags, _, _, _, _, _, _, _, dataDir, _, _, _, _, _, _, _, _, _, configPath := newDoctorFlags()
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("flags.Parse: %v", err)
	}
	got, err := notifyTestClickTarget(flags, *dataDir, *configPath)
	if err != nil || got != "/configured/data" {
		t.Errorf("notifyTestClickTarget with no -data-dir = %q, %v, want the session config's data_dir", got, err)
	}

	flags, _, _, _, _, _, _, _, dataDir, _, _, _, _, _, _, _, _, _, configPath = newDoctorFlags()
	if err := flags.Parse([]string{"-data-dir", "relative/data"}); err != nil {
		t.Fatalf("flags.Parse: %v", err)
	}
	got, err = notifyTestClickTarget(flags, *dataDir, *configPath)
	if err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "data" {
		t.Errorf("notifyTestClickTarget(-data-dir relative/data) = %q, %v, want that directory as an absolute path", got, err)
	}
}
