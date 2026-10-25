package modelrole

import (
	"testing"
)

func TestMeterMaxRequestBytesDerivation(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		name       string
		configured int64
		window     int
		want       int64
	}{
		{"large window hits the binding cap", 0, 272000, 4 * mib},
		{"mid window is window x 32", 0, 100000, 3200000},
		{"no window", 0, 0, 4 * mib},
		{"small window floors at 1 MiB", 0, 1000, mib},
		{"explicit wins", 2097152, 272000, 2097152},
		{"explicit below floor wins", 4096, 272000, 4096},
	} {
		if got := meterMaxRequestBytes(c.configured, c.window); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRoutePolicyMaxRequestBytesFollowsTheModel(t *testing.T) {
	s := routesModeSettings()
	route := s.Routes["primary"]
	model := s.Models["luna"]
	model.ContextWindow = 100000

	p, err := relayPolicyForRoute(s, model, "primary", route)
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxRequestBytes != 3200000 {
		t.Fatalf("derived MaxRequestBytes = %d, want 3200000", p.MaxRequestBytes)
	}

	s.MeterMaxRequestBytes = 2097152
	p, err = relayPolicyForRoute(s, model, "primary", route)
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxRequestBytes != 2097152 {
		t.Fatalf("explicit MaxRequestBytes = %d, want 2097152", p.MaxRequestBytes)
	}
}
