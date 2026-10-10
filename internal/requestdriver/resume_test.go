package requestdriver_test

import (
	"strings"
	"testing"

	"buildgate/internal/requestdriver"
)

func TestValidateResumeWorktreeFlags(t *testing.T) {
	cases := []struct {
		name                  string
		of                    string
		onBranch, repo, prior string
		wantErr               string
	}{
		{name: "unset is always fine"},
		{name: "ok", of: "r1"},
		{name: "on-branch", of: "r1", onBranch: "x", wantErr: "-on-branch"},
		{name: "repository", of: "r1", repo: "o/r", wantErr: "-repository"},
		{name: "prior-run", of: "r1", prior: "p", wantErr: "-prior-run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requestdriver.ValidateResumeWorktreeFlags(tc.of, tc.onBranch, tc.repo, tc.prior)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
