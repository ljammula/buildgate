package requestdriver_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// waitingAt saves a request that entered state long ago.
func waitingAt(t *testing.T, state request.State, waited time.Duration) (dataDir, id string) {
	t.Helper()
	dataDir, id = t.TempDir(), "req-1"
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, id, state)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.EnteredAt = time.Now().Add(-waited).UTC().Format(time.RFC3339Nano)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir, id
}

func TestExpireIfDueHaltsAReviewPastItsExpiry(t *testing.T) {
	for _, state := range []request.State{request.StateSpecReview, request.StateOracleReview, request.StatePlanReview} {
		dataDir, id := waitingAt(t, state, 3*time.Hour)
		expired, err := requestdriver.ExpireIfDue(dataDir, id, 2*time.Hour, time.Now())
		if err != nil || !expired {
			t.Fatalf("%s: expired %v err %v, want the halt", state, expired, err)
		}
		got := requestdrivertest.LoadRequest(t, dataDir, id)
		want := string(state) + " expired: no decision in 2h0m0s (review_gate_expiry)"
		if got.State != request.StateHalted || got.HaltKind != request.HaltGateExpired || got.Error != want {
			t.Errorf("%s: state %q kind %q error %q, want halted gate_expired %q", state, got.State, got.HaltKind, got.Error, want)
		}
		// The halt is notified like any other.
		log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, id))
		if err != nil || !strings.Contains(string(log), "expired") {
			t.Errorf("%s: notifications.log = %q (%v), want the expiry told", state, log, err)
		}
	}
	dataDir, id := waitingAt(t, request.StatePlanReview, time.Hour)
	if expired, err := requestdriver.ExpireIfDue(dataDir, id, 2*time.Hour, time.Now()); err != nil || expired {
		t.Errorf("within the expiry: expired %v err %v, want neither", expired, err)
	}
}

func TestExpireIfDueOffByDefault(t *testing.T) {
	dataDir, id := waitingAt(t, request.StateSpecReview, 1000*time.Hour)
	if expired, err := requestdriver.ExpireIfDue(dataDir, id, 0, time.Now()); err != nil || expired {
		t.Fatalf("expiry 0: expired %v err %v, want a gate that never expires", expired, err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateSpecReview {
		t.Errorf("state = %q, want spec_review", got.State)
	}
}

func TestExpiryNeverAppliesToPRReview(t *testing.T) {
	for _, state := range []request.State{request.StatePRReview, request.StateBuilding, request.StateQuarantined} {
		dataDir, id := waitingAt(t, state, 1000*time.Hour)
		if expired, err := requestdriver.ExpireIfDue(dataDir, id, time.Minute, time.Now()); err != nil || expired {
			t.Errorf("%s: expired %v err %v, want no expiry", state, expired, err)
		}
		if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != state {
			t.Errorf("%s: state = %q after ExpireIfDue", state, got.State)
		}
	}
}

func TestExpireIfDueLeavesADecisionMadeMeanwhile(t *testing.T) {
	dataDir, id := waitingAt(t, request.StateSpecReview, 3*time.Hour)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := r.Halt("some other halt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if expired, err := requestdriver.ExpireIfDue(dataDir, id, time.Hour, time.Now()); err != nil || expired {
		t.Errorf("a request already halted: expired %v err %v", expired, err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.HaltKind != "" {
		t.Errorf("HaltKind = %q, want the earlier halt untouched", got.HaltKind)
	}
}
