package request

import (
	"fmt"

	"buildgate/internal/ticketspec"
)

// RefuseBugTestsOptOut is the structural check on ticket 1 of a bug request
// (Request.Bug): it must add a test that fails on the base commit and passes
// on the result, so its spec may not declare `Tests-Required: no`. It reads
// the ticket's header line only. A ticket file that does not parse is the
// parse error, not a refusal here.
func RefuseBugTestsOptOut(ticketSpecPath string) error {
	reason, err := ticketspec.ParseTestsRequiredOptOut(ticketSpecPath)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("a bug request's ticket 1 must add a test that fails on the base commit and passes on the result, and declares %s no -- %s: remove the opt-out", ticketspec.TestsRequiredPrefix, reason)
	}
	return nil
}
