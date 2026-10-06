package notify

import (
	"os"
	"testing"
)

// TestMain opts this package's tests out of desktop notifications, which
// are on by default. Tests that exercise the desktop channel opt back in
// per test with t.Setenv and inject a stand-in for osascript, so nothing
// here ever shows a real notification.
//
// The notification icon cache is redirected to a throwaway directory for
// the same reason: no test run writes into the real user cache.
func TestMain(m *testing.M) {
	os.Setenv(DesktopNotificationsEnvironmentVariable, "0")
	cacheDir, err := os.MkdirTemp("", "notify-icon-cache-")
	if err != nil {
		panic(err)
	}
	desktopIconCacheDir = func() (string, error) { return cacheDir, nil }
	code := m.Run()
	os.RemoveAll(cacheDir)
	os.Exit(code)
}
