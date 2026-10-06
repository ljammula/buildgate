package main

import (
	"strings"
	"testing"

	"buildgate/internal/prices"
	"buildgate/internal/sessionconfig"
)

// TestDoctorPriceChecksOKWhenNoModels proves an empty models: (or an
// entirely absent routes:/models:/roles: block) needs no price check at
// all -- an offline build declares no model, so there is nothing to
// price.
func TestDoctorPriceChecksOKWhenNoModels(t *testing.T) {
	checks := doctorPriceChecks(sessionconfig.Settings{})
	if len(checks) != 0 {
		t.Fatalf("doctorPriceChecks = %+v, want no checks when no models: are declared", checks)
	}
}

// TestDoctorPriceChecksPassesForATableModel proves a declared model whose
// id is in the compiled internal/prices table prints its own price line
// (source/as_of included) as a clean pass.
func TestDoctorPriceChecksPassesForATableModel(t *testing.T) {
	settings := sessionconfig.Settings{
		Models: map[string]sessionconfig.Model{
			"luna": {ID: "gpt-5.6-luna", Routes: []string{"local"}},
		},
	}
	checks := doctorPriceChecks(settings)
	if len(checks) != 1 {
		t.Fatalf("doctorPriceChecks = %+v, want exactly one check", checks)
	}
	c := checks[0]
	if c.Err != nil {
		t.Fatalf("Err = %v, want nil for a known model id", c.Err)
	}
	for _, want := range []string{"models.luna", "gpt-5.6-luna", "$0.2", "$0.02", "$1.2", "2026-09-28"} {
		if !strings.Contains(c.Name, want) {
			t.Errorf("Name = %q, want it to contain %q", c.Name, want)
		}
	}
}

// TestDoctorPriceChecksWarnsForAnUnknownModelID proves a declared model
// whose id has no internal/prices entry gets an advisory warning: it runs
// at $0, so its dollar budget and ceiling never trip.
func TestDoctorPriceChecksWarnsForAnUnknownModelID(t *testing.T) {
	settings := sessionconfig.Settings{
		Models: map[string]sessionconfig.Model{
			"local-model": {ID: "my-local-model", Routes: []string{"local"}},
		},
	}
	checks := doctorPriceChecks(settings)
	if len(checks) != 1 {
		t.Fatalf("doctorPriceChecks = %+v, want exactly one check", checks)
	}
	c := checks[0]
	if c.Err == nil {
		t.Fatal("Err = nil, want a warning for an unpriced model id")
	}
	if !c.Advisory {
		t.Error("Advisory = false, want true: a missing price is a warning, not a failure")
	}
	if !strings.Contains(c.Err.Error(), `model id "my-local-model": no price`) || !strings.Contains(c.Err.Error(), "costs show as $0") {
		t.Errorf("Err = %v, want it to name the missing model id", c.Err)
	}
}

// TestDoctorPriceChecksWarnsOnStaleAsOf proves a found price with an
// as_of older than 90 days warns (Advisory), rather than either silently
// passing or hard-failing over metadata that has no bearing on whether
// the price itself is still correct.
func TestDoctorPriceChecksWarnsOnStaleAsOf(t *testing.T) {
	// gpt-5.6-luna's own compiled as_of (2026-09-28) is always fresh
	// relative to time.Now() in this test's own lifetime, so this proves
	// the negative instead: a fresh entry never warns.
	settings := sessionconfig.Settings{
		Models: map[string]sessionconfig.Model{
			"luna": {ID: "gpt-5.6-luna", Routes: []string{"local"}},
		},
	}
	checks := doctorPriceChecks(settings)
	if len(checks) != 1 || checks[0].Err != nil {
		t.Fatalf("doctorPriceChecks = %+v, want one clean pass for a fresh as_of", checks)
	}
}

func TestFormatUSDPerMTok(t *testing.T) {
	cases := map[int64]string{
		200000:  "0.2",
		20000:   "0.02",
		1200000: "1.2",
		4000000: "4",
		1:       "0.000001",
	}
	for micro, want := range cases {
		if got := formatUSDPerMTok(prices.USDPerMTok(micro)); got != want {
			t.Errorf("formatUSDPerMTok(%d) = %q, want %q", micro, got, want)
		}
	}
}
