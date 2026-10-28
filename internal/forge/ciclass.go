package forge

import "strings"

// claDCOPosters is the fixed list of apps (check run app slugs) and
// contexts (commit status contexts) that post checks running none of the
// change's code: CLA and DCO checks. Exact, lower-cased compares of
// Check.Poster. A failure of one is never the code's to fix.
var claDCOPosters = map[string]bool{
	"dco": true, "dco/probot": true,
	"cla-assistant": true, "cla-assistant-io": true, "cla-bot": true,
	"easycla": true, "linux-foundation-easycla": true,
	"license/cla": true, "cla/google": true, "cla/linuxfoundation": true,
	"cla/microsoft": true, "cla/assistant": true,
}

// IsCLAOrDCO reports whether c was posted by an app or context on the fixed
// CLA/DCO list.
func (c Check) IsCLAOrDCO() bool {
	return claDCOPosters[strings.ToLower(strings.TrimSpace(c.Poster))]
}

// CIVerdict is the sorting of a PR head's checks (safety-contract.md, the
// paragraph on CI checks).
type CIVerdict struct {
	// Pending are the checks not yet completed that are, or may be,
	// required: what the request waits for.
	Pending []Check
	// Notes are completed checks that did not succeed and are not required.
	Notes []Check
	// Rounds are required (or, with the required set unread, any) checks
	// that completed with FAILURE, none posted by a CLA/DCO app: candidates
	// for a CI round.
	Rounds []Check
	// Triage are required checks whose failure is not the code's: a
	// conclusion other than FAILURE, or a CLA/DCO poster. They start no
	// round and go to the operator.
	Triage []Check
}

// ClassifyChecks sorts checks by their structured fields only, never a log
// line. With requiredUnknown every completed FAILURE counts as required and
// any other non-success is a note.
func ClassifyChecks(checks []Check, requiredUnknown bool) CIVerdict {
	var v CIVerdict
	for _, c := range checks {
		notRequired := !requiredUnknown && (c.Required == nil || !*c.Required)
		if !c.Completed {
			if !notRequired {
				v.Pending = append(v.Pending, c)
			}
			continue
		}
		if checkPassingStates[c.Conclusion] {
			continue
		}
		switch {
		case notRequired:
			v.Notes = append(v.Notes, c)
		case c.IsCLAOrDCO():
			v.Triage = append(v.Triage, c)
		case c.Conclusion == "FAILURE":
			v.Rounds = append(v.Rounds, c)
		case requiredUnknown:
			v.Notes = append(v.Notes, c)
		default:
			v.Triage = append(v.Triage, c)
		}
	}
	return v
}
