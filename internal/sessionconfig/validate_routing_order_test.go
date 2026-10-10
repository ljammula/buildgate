package sessionconfig

import (
	"strings"
	"testing"
)

// TestValidateRoutingNamesTheFirstOffendingEntryInSortedOrder: when several
// routes:, models: or route_ids entries are wrong at once, the error names
// the same one every time (the first in sorted order), not whichever a map
// iteration reached first.
func TestValidateRoutingNamesTheFirstOffendingEntryInSortedOrder(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie", "delta", "echo"}

	t.Run("routes", func(t *testing.T) {
		s := routesModeSettings()
		for _, name := range names {
			s.Routes[name] = Route{CredentialMode: "made-up-mode"}
		}
		err := ValidateRouting(s)
		if err == nil || !strings.Contains(err.Error(), "alpha") {
			t.Errorf("ValidateRouting = %v, want a refusal naming route alpha", err)
		}
	})

	t.Run("models", func(t *testing.T) {
		s := routesModeSettings()
		for _, name := range names {
			s.Models[name] = Model{Routes: []string{"litellm"}}
		}
		err := ValidateRouting(s)
		if err == nil || !strings.Contains(err.Error(), "models.alpha: id is required") {
			t.Errorf("ValidateRouting = %v, want models.alpha: id is required", err)
		}
	})

	t.Run("route_ids", func(t *testing.T) {
		s := routesModeSettings()
		routeIDs := map[string]string{}
		for _, name := range names {
			routeIDs[name] = "some-id"
		}
		s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, RouteIDs: routeIDs}
		err := ValidateRouting(s)
		if err == nil || !strings.Contains(err.Error(), `route_ids names "alpha"`) {
			t.Errorf("ValidateRouting = %v, want a refusal naming route_ids alpha", err)
		}
	})
}
