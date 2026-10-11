package run

import "buildgate/internal/meter"

// AttemptCalls is calls as an Attempt records it: nil when the launch made
// no model request, so an attempt that called no model carries no field.
func AttemptCalls(calls meter.Calls) *meter.Calls {
	if calls.Recorded() == 0 {
		return nil
	}
	return &calls
}
