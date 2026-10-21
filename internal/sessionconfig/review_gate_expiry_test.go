package sessionconfig

import (
	"strings"
	"testing"
	"time"
)

func TestReviewGateExpiryIsOffByDefaultAndParsesADuration(t *testing.T) {
	if got := DefaultSettings().ReviewGateExpiry; got != 0 {
		t.Errorf("default ReviewGateExpiry = %v, want 0 (a gate never expires)", got)
	}
	cfg, err := Load(writeConfig(t, "review_gate_expiry: 36h\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if s.ReviewGateExpiry != 36*time.Hour {
		t.Errorf("ReviewGateExpiry = %v, want 36h", s.ReviewGateExpiry)
	}
	unset, err := Load(writeConfig(t, "hitl_reminder_interval: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := unset.ApplySettings(DefaultSettings()); err != nil || s.ReviewGateExpiry != 0 {
		t.Errorf("a config without the key: ReviewGateExpiry = %v err %v, want 0", s.ReviewGateExpiry, err)
	}
}

func TestReviewGateExpiryRefusesWhatIsNotADuration(t *testing.T) {
	for _, body := range []string{"review_gate_expiry: soon\n", "review_gate_expiry: -1h\n"} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "review_gate_expiry") {
			t.Errorf("%q: ApplySettings = %v, want a refusal naming review_gate_expiry", body, err)
		}
	}
}
