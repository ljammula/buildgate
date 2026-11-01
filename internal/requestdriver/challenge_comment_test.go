package requestdriver

import (
	"strings"
	"testing"

	"buildgate/internal/run"
)

// A held file the round gave no verdict (broken, or absent from its record)
// is not listed as one that still fails.
func TestChallengeCommentListsAFileWithNoVerdictApart(t *testing.T) {
	reds := []run.ChallengeFile{
		{Path: "p/a_test.go", Criterion: "AC-1"},
		{Path: "p/b_test.go", Criterion: "AC-2"},
		{Path: "p/c_test.go", Criterion: "AC-3"},
		{Path: "p/d_test.go", Criterion: "AC-4"},
	}
	round := &run.Run{Challenge: &run.ChallengeRecord{Verdict: run.ChallengeRed, Files: []run.ChallengeFile{
		{Path: "p/a_test.go", Verdict: run.ChallengeGreen},
		{Path: "p/b_test.go", Verdict: run.ChallengeRed},
		{Path: "p/c_test.go", Verdict: "broken"},
	}}}
	got := challengeComment(reds, &run.Run{ResultSHA: "abcdef1234567890"}, round)
	fail, none, ok := strings.Cut(got, "The round gave no verdict on these held challenge tests:")
	if !ok {
		t.Fatalf("no separate no-verdict list:\n%s", got)
	}
	if !strings.Contains(fail, "still fail on it") || !strings.Contains(fail, "b_test.go") || strings.Contains(fail, "c_test.go") || strings.Contains(fail, "d_test.go") {
		t.Errorf("the still-failing part:\n%s", fail)
	}
	if !strings.Contains(none, "c_test.go") || !strings.Contains(none, "d_test.go") || strings.Contains(none, "b_test.go") {
		t.Errorf("the no-verdict part:\n%s", none)
	}
}
