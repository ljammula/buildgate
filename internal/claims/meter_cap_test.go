package claims

import (
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/sessionconfig"
)

// TestSessionConfigAndMeterAgreeOnTheGatewayRequestCap: the session config
// refuses a meter_max_request_bytes above sessionconfig.GatewayMaxRequestBytes,
// and the meter's own policy caps the same value at meter.DefaultMaxRequestBytes.
// internal/meter imports no other buildgate package, so the two constants are
// kept equal here: if they drifted, a value the config accepts could be one the
// meter or the gateway refuses, and every sandbox would fail to start.
func TestSessionConfigAndMeterAgreeOnTheGatewayRequestCap(t *testing.T) {
	if sessionconfig.GatewayMaxRequestBytes != meter.DefaultMaxRequestBytes {
		t.Fatalf("sessionconfig.GatewayMaxRequestBytes = %d, meter.DefaultMaxRequestBytes = %d: they must be the same cap", sessionconfig.GatewayMaxRequestBytes, meter.DefaultMaxRequestBytes)
	}
}
