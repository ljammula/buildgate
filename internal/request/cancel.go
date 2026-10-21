package request

import "errors"

// ErrCancelled is the cause a step's context carries when the request was
// cancelled while the step ran (context.Cause): the step stops, the sandbox it
// started is removed, and the run it was building is rolled back instead of
// kept for a resume. The run halts with run.HaltReasonRequestCancelled.
var ErrCancelled = errors.New("request cancelled")
