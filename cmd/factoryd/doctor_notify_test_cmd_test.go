package main

import (
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
