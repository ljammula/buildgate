package main

import (
	"testing"

	"buildgate/internal/workflow"
)

// TestRequestsAheadOf covers requestsAheadOf's pure "how many other
// requests are ahead of mine" computation against a hand-built
// workflow.RepositoryOwnerResult, per progress-contract.md's "queued"
// stage (2026-09-18): pollRepositoryOwnerResult emits a queued progress
// note only when this count is at least one and has changed since the
// last poll.
func TestRequestsAheadOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		result    workflow.RepositoryOwnerResult
		requestID string
		want      int
	}{
		{
			name:      "nothing in progress or queued",
			result:    workflow.RepositoryOwnerResult{},
			requestID: "r1",
			want:      0,
		},
		{
			name: "own request is the one in progress",
			result: workflow.RepositoryOwnerResult{
				InProgress: &workflow.InProgressRun{RequestID: "r1"},
			},
			requestID: "r1",
			want:      0,
		},
		{
			name: "another request in progress, none queued",
			result: workflow.RepositoryOwnerResult{
				InProgress: &workflow.InProgressRun{RequestID: "other"},
			},
			requestID: "r1",
			want:      0,
		},
		{
			name: "queued behind the in-progress request alone",
			result: workflow.RepositoryOwnerResult{
				InProgress:       &workflow.InProgressRun{RequestID: "other"},
				QueuedRequestIDs: []string{"r1"},
			},
			requestID: "r1",
			want:      1,
		},
		{
			name: "queued behind two others, none in progress",
			result: workflow.RepositoryOwnerResult{
				QueuedRequestIDs: []string{"a", "b", "r1", "c"},
			},
			requestID: "r1",
			want:      2,
		},
		{
			name: "in progress plus two queued ahead",
			result: workflow.RepositoryOwnerResult{
				InProgress:       &workflow.InProgressRun{RequestID: "other"},
				QueuedRequestIDs: []string{"a", "b", "r1"},
			},
			requestID: "r1",
			want:      3,
		},
		{
			name: "already completed, not in progress or queued",
			result: workflow.RepositoryOwnerResult{
				Runs: map[string]workflow.RunWorkflowResult{"r1": {State: "accepted"}},
			},
			requestID: "r1",
			want:      0,
		},
		{
			name: "not yet visible to the owner at all",
			result: workflow.RepositoryOwnerResult{
				InProgress:       &workflow.InProgressRun{RequestID: "other"},
				QueuedRequestIDs: []string{"a"},
			},
			requestID: "r1",
			want:      0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := requestsAheadOf(c.result, c.requestID); got != c.want {
				t.Errorf("requestsAheadOf() = %d, want %d", got, c.want)
			}
		})
	}
}
