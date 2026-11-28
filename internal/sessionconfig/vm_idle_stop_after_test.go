package sessionconfig

import (
	"strings"
	"testing"
	"time"
)

func TestVMIdleStopAfterIsOffByDefaultAndTakesADuration(t *testing.T) {
	if got := DefaultSettings().VMIdleStopAfter; got != 0 {
		t.Errorf("default VMIdleStopAfter = %v, want 0 (the VM is never stopped for being idle)", got)
	}
	for body, want := range map[string]time.Duration{
		"vm_idle_stop_after: 30m\n": 30 * time.Minute,
		"vm_idle_stop_after: 5m\n":  5 * time.Minute,
		"vm_idle_stop_after: 2h\n":  2 * time.Hour,
		"vm_idle_stop_after: 0\n":   0,
	} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if s.VMIdleStopAfter != want {
			t.Errorf("%q: VMIdleStopAfter = %v, want %v", body, s.VMIdleStopAfter, want)
		}
	}
}

func TestVMIdleStopAfterRefusesWhatIsNotZeroOrAtLeastFiveMinutes(t *testing.T) {
	for _, body := range []string{"vm_idle_stop_after: soon\n", "vm_idle_stop_after: 1m\n", "vm_idle_stop_after: -1h\n", "vm_idle_stop_after: \"\"\n", "vm_idle_stop_after: 1d\n"} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			continue
		}
		if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "vm_idle_stop_after") {
			t.Errorf("%q: ApplySettings = %v, want a refusal naming vm_idle_stop_after", body, err)
		}
	}
}
