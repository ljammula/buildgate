package api

import (
	"os"
	"testing"

	"buildgate/internal/notify"
)

// TestMain opts this package's tests out of desktop notifications, which
// are on by default: the halt paths under test call notify.DispatchExternal
// in-process, and a `go test` on a Mac must never show a real notification.
func TestMain(m *testing.M) {
	os.Setenv(notify.DesktopNotificationsEnvironmentVariable, "0")
	os.Exit(m.Run())
}
