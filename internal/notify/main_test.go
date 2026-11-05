package notify

import (
	"os"
	"testing"
)

// TestMain opts this package's tests out of desktop notifications, which
// are on by default. Tests that exercise the desktop channel opt back in
// per test with t.Setenv and inject a stand-in for osascript, so nothing
// here ever shows a real notification.
// Each of those hands its DesktopNotifier a testDesktopEnv, whose icon cache
// is a throwaway directory: no test run writes into the real user cache.
func TestMain(m *testing.M) {
	os.Setenv(DesktopNotificationsEnvironmentVariable, "0")
	os.Exit(m.Run())
}
