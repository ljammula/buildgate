package requestdriver_test

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"

	"buildgate/internal/notify"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// A request's notification links to the console base its Deps resolved, not
// to whatever the machine running the test would resolve.
func TestRequestNotificationLinkUsesTheDepsConsoleBaseURL(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(notify.DesktopNotificationsEnvironmentVariable, "0")
	r := &request.Request{ID: "req-1", Project: "checkouts", State: request.StateSpecReview, TicketCount: 1}
	if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(request.TextPath(dataDir, r.ID), []byte("Add a coupon field\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	dp := newFakeDeps(t)
	var askedFor string
	dp.consoleBaseURLFn = func(dir string) string {
		askedFor = dir
		return "http://console.test"
	}

	requestdriver.RemindRequest(dp, dataDir, r, time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC))

	if askedFor != dataDir {
		t.Errorf("ConsoleBaseURL asked for %q, want %q", askedFor, dataDir)
	}
	f, err := os.Open(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []notify.Notification
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var n notify.Notification
		if err := json.Unmarshal(scanner.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if len(got) != 1 {
		t.Fatalf("logged %d notifications, want 1", len(got))
	}
	if want := "http://console.test/requests/req-1"; got[0].Link != want {
		t.Errorf("Link = %q, want %q", got[0].Link, want)
	}
}
