package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadMeterBytes(t *testing.T, body string) (Settings, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		return Settings{}, err
	}
	return cfg.ApplySettings(DefaultSettings())
}

func TestMeterMaxRequestBytesIsUnsetByDefaultAndReadWhenSet(t *testing.T) {
	if got := DefaultSettings().MeterMaxRequestBytes; got != 0 {
		t.Fatalf("default = %d, want 0 (derive per model)", got)
	}
	s, err := loadMeterBytes(t, "meter_max_request_bytes: 2097152\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.MeterMaxRequestBytes != 2097152 {
		t.Fatalf("MeterMaxRequestBytes = %d, want 2097152", s.MeterMaxRequestBytes)
	}
}

func TestMeterMaxRequestBytesRefusesNonPositive(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		_, err := loadMeterBytes(t, "meter_max_request_bytes: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "meter_max_request_bytes: must be at least 1") {
			t.Fatalf("value %s: err = %v, want a refusal", v, err)
		}
	}
}

func TestMeterMaxRequestBytesRefusesAboveTheGatewayCap(t *testing.T) {
	_, err := loadMeterBytes(t, "meter_max_request_bytes: 8388608\n")
	if err == nil || !strings.Contains(err.Error(), "4 MiB") {
		t.Fatalf("err = %v, want a refusal naming the 4 MiB cap", err)
	}
	if _, err := loadMeterBytes(t, "meter_max_request_bytes: 4194304\n"); err != nil {
		t.Fatalf("4194304 refused: %v", err)
	}
}
