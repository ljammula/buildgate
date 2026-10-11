// Package unresolved names the ways a step can end with the factory holding
// no verdict on the work.
package unresolved

import "fmt"

// Cause names why a step ended with the factory holding no verdict
// on the work: something outside the work failed. It is a closed set, each
// member decided from a fact the factory itself observed (a Temporal timeout,
// a sandbox whose command was never released), never from a worker's or a provider's
// wording. An unresolved step accepts nothing and judges nothing: its request
// waits in resume_review for a human, and its round is not counted.
//
// A halted run records its cause as run.Run.HaltReasonCode; a request records
// it as request.ResumeInfo.Cause. Adding a cause is one constant and one
// entry in causes.
type Cause string

const (
	// WorkerLost: the worker running the step went away (a
	// heartbeat or start-to-close timeout, a dead owner process, or a
	// command the sandbox runtime started twice).
	WorkerLost Cause = "worker_lost"
	// SandboxLaunch: the step's sandbox never started its command (it
	// could not be created, or had no worker container), and is confirmed
	// gone. A command that started and then hung, was cut off or printed
	// too much is not one: it ran.
	SandboxLaunch Cause = "sandbox_launch"
	// ModelUnavailable: the meter's ledger shows the step's model requests
	// were never served (no answer at all, or none after the last one that
	// was): the route did not respond, or failed.
	ModelUnavailable Cause = "model_unavailable"
	// ProviderQuota: as ModelUnavailable, and the provider refused with its
	// own rate or usage limit (HTTP 429).
	ProviderQuota Cause = "provider_quota"
	// MeterDenied: as ModelUnavailable, and the factory's own spend meter
	// refused the requests at a ceiling, a window budget or the request
	// rate. The session config's settings govern it.
	MeterDenied Cause = "meter_denied"
	// NoVerdict: a review's model answered and the review still produced no
	// verdict the factory could read, after its one re-ask.
	NoVerdict Cause = "no_verdict"
	// DependencyFetch: a repository command failed in an attempt during
	// which the registry proxy could not reach a package registry.
	DependencyFetch Cause = "dependency_fetch"
	// DraftingJob: a drafting step's job ended before its script reached a
	// result of its own (the launch returned no exit code, or the script
	// exited non-zero leaving no evidence file), and nothing more specific
	// is known. A draft the script or the factory's checks refused is not
	// one: that is a result.
	DraftingJob Cause = "drafting_job"
)

// causes holds, for each cause, the operator's clause about what
// happened; %s is the step.
var causes = map[Cause]string{
	WorkerLost:       "the factoryd worker stopped while the %s step ran",
	SandboxLaunch:    "the sandbox of the %s step could not be started, so the step never ran",
	ModelUnavailable: "the model route served none of the last requests of the %s step, so the work was not judged",
	ProviderQuota:    "the model provider refused the requests of the %s step with its own rate or usage limit, so the work was not judged",
	MeterDenied:      "the spend meter refused the model requests of the %s step (a ceiling, a window budget or the request rate in the session config; `factoryd cost` shows the spend), so the work was not judged",
	NoVerdict:        "the review of the %s step answered and gave no verdict the factory could read",
	DependencyFetch:  "a package registry could not be reached while the %s step ran, so its command's failure says nothing about the work",
	DraftingJob:      "the job of the %s step ended before it produced a draft, so there is nothing to review",
}

// Known reports whether c is a member of the closed set.
func (c Cause) Known() bool {
	_, ok := causes[c]
	return ok
}

// What is the operator's clause about what happened to step. A cause outside
// the set reads as a lost worker: a record written before causes existed
// could only have been one.
func (c Cause) What(step string) string {
	clause, ok := causes[c]
	if !ok {
		clause = causes[WorkerLost]
	}
	return fmt.Sprintf(clause, step)
}
