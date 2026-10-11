package unresolved

import (
	"strings"
	"testing"
)

func TestEveryCauseHasAClauseThatNamesTheStep(t *testing.T) {
	for cause, clause := range causes {
		if !cause.Known() {
			t.Errorf("%q is in the table and not Known", cause)
		}
		if strings.Count(clause, "%s") != 1 {
			t.Errorf("%q: clause %q must name the step exactly once", cause, clause)
		}
		if got := cause.What("review"); !strings.Contains(got, "review") || strings.Contains(got, "%") {
			t.Errorf("%q.What(review) = %q", cause, got)
		}
	}
}

// A request record written before causes existed has none: it could only
// have been a lost worker, and must keep reading as one.
func TestACauseOutsideTheSetReadsAsALostWorker(t *testing.T) {
	for _, cause := range []Cause{"", "review_unavailable", "anything a worker wrote"} {
		if cause.Known() {
			t.Errorf("%q is Known", cause)
		}
		if got, want := cause.What("build"), WorkerLost.What("build"); got != want {
			t.Errorf("%q.What = %q, want the lost worker's %q", cause, got, want)
		}
	}
}
