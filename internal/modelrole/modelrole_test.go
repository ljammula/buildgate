package modelrole

import (
	"testing"
)

// TestForStageCoversEveryStage is the exhaustive table test: every Stage
// constant this package declares must map to a Role, and the mapping must
// match the PR spec's own stage->role grouping.
func TestForStageCoversEveryStage(t *testing.T) {
	cases := []struct {
		stage Stage
		want  Role
	}{
		{StageSpecDrafting, RolePlanning},
		{StagePlanning, RolePlanning},
		{StageBuilding, RoleExecution},
		{StageConformityCorrective, RoleExecution},
		{StagePRCorrective, RoleExecution},
		{StageSpecConformity, RoleReview},
		{StageOracleDrafting, RoleReview},
	}
	for _, c := range cases {
		if got := ForStage(c.stage); got != c.want {
			t.Errorf("ForStage(%q) = %q, want %q", c.stage, got, c.want)
		}
	}
}

func TestForStageUnknownStagePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("ForStage(unknown): want a panic")
		}
	}()
	ForStage(Stage("no_such_stage"))
}
