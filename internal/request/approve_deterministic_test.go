package request

import (
	"errors"
	"strings"
	"testing"
)

// TestApprovalRefusalNamesTheFirstOffendingFileInSortedOrder: when several
// files are wrong at once, the refusal names the same one every time (the
// first in sorted order), not whichever a map iteration reached first.
func TestApprovalRefusalNamesTheFirstOffendingFileInSortedOrder(t *testing.T) {
	hashes := map[string]string{}
	for _, rel := range []string{
		"tickets/001.spec.md", "tickets/002.spec.md", "tickets/003.spec.md", "tickets/004.spec.md",
	} {
		hashes[rel] = "current-" + rel
	}

	t.Run("stale files", func(t *testing.T) {
		expected := map[string]string{}
		for rel := range hashes {
			expected[rel] = "stale"
		}
		err := checkApprovalAgainstExpected("req-1", hashes, expected)
		if !errors.Is(err, ErrApprovalStale) {
			t.Fatalf("err = %v, want ErrApprovalStale", err)
		}
		if !strings.Contains(err.Error(), "request req-1: tickets/001.spec.md ") {
			t.Errorf("err = %v, want it to name tickets/001.spec.md", err)
		}
	})
}
