package sessionconfig

import (
	"strings"
	"testing"
	"time"
)

func TestPruneAfterIsOffByDefaultAndTakesDaysOrADuration(t *testing.T) {
	if got := DefaultSettings().PruneAfter; got != 0 {
		t.Errorf("default PruneAfter = %v, want 0 (the worker prunes nothing)", got)
	}
	for body, want := range map[string]time.Duration{
		"prune_after: 14d\n":            14 * 24 * time.Hour,
		"prune_after: 36h\n":            36 * time.Hour,
		"hitl_reminder_interval: 10m\n": 0,
	} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil {
			t.Fatalf("ApplySettings %q: %v", body, err)
		}
		if s.PruneAfter != want {
			t.Errorf("%q: PruneAfter = %v, want %v", body, s.PruneAfter, want)
		}
	}
}

func TestPruneAfterRefusesWhatIsNotAPositiveAge(t *testing.T) {
	for _, body := range []string{"prune_after: soon\n", "prune_after: 0d\n", "prune_after: -1h\n", "prune_after: 1.5d\n", "prune_after: \"\"\n"} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "prune_after") {
			t.Errorf("%q: ApplySettings = %v, want a refusal naming prune_after", body, err)
		}
	}
}

func TestPruneAfterAndOlderThanHaveAFloorOf24Hours(t *testing.T) {
	for _, bad := range []string{"1h", "23h", "90m", "0d"} {
		if _, err := ParseAge(bad); err == nil || !strings.Contains(err.Error(), "at least 24h") && bad != "0d" {
			t.Errorf("ParseAge(%q) = %v, want the 24h floor refusal", bad, err)
		}
		cfg, err := Load(writeConfig(t, "prune_after: "+bad+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "prune_after") {
			t.Errorf("prune_after %s: %v, want a refusal", bad, err)
		}
	}
	for _, ok := range []string{"24h", "1d", "30d"} {
		if _, err := ParseAge(ok); err != nil {
			t.Errorf("ParseAge(%q) = %v", ok, err)
		}
	}
}
