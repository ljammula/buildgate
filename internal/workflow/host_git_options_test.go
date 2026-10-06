package workflow

import (
	"testing"
	"time"
)

// TestHostGitActivityOptionsFailFast pins the 10-minute StartToClose of the
// two non-heartbeating host-side git Activities (PostBuild, CollectEvidence).
func TestHostGitActivityOptionsFailFast(t *testing.T) {
	o := hostGitActivityOptions()
	if o.StartToCloseTimeout != 10*time.Minute {
		t.Errorf("StartToCloseTimeout = %v, want 10m", o.StartToCloseTimeout)
	}
	if o.RetryPolicy == nil || o.RetryPolicy.MaximumAttempts != 1 {
		t.Errorf("RetryPolicy = %+v, want single attempt", o.RetryPolicy)
	}
}
