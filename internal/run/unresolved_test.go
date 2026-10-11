package run

import (
	"buildgate/internal/unresolved"
	"testing"
)

func TestUnresolvedCauseOfReadsOnlyAHaltedRunsOwnRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *Run
		want unresolved.Cause
	}{
		{"nil", nil, ""},
		{"a launch failure", &Run{State: StateHalted, HaltReasonCode: string(unresolved.SandboxLaunch)}, unresolved.SandboxLaunch},
		{"a lost worker on a branch round, nothing kept", &Run{State: StateHalted, HaltReasonCode: string(unresolved.WorkerLost)}, unresolved.WorkerLost},
		{"kept before causes were recorded", &Run{State: StateHalted, KeptForResume: true}, unresolved.WorkerLost},
		{"a resumed run that halted on a result keeps its worktree and is judged", &Run{State: StateHalted, KeptForResume: true, HaltReasonCode: HaltReasonRelayCeilingExceeded}, ""},
		{"a halt with a result", &Run{State: StateHalted, HaltReasonCode: HaltReasonBaselineVerifyFailed}, ""},
		{"an ordinary halt", &Run{State: StateHalted, HaltError: "sandbox attempts exhausted"}, ""},
		{"a quarantined run is a verdict whatever its code says", &Run{State: StateQuarantined, HaltReasonCode: string(unresolved.SandboxLaunch)}, ""},
		{"an accepted run", &Run{State: StateAccepted, KeptForResume: true}, ""},
	} {
		if got := UnresolvedCauseOf(tc.run); got != tc.want {
			t.Errorf("%s: UnresolvedCauseOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}
