package modelrole

import (
	"encoding/json"
	"reflect"
	"testing"

	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// TestSelectionCannotCarryACredential reflect-walks Selection (and through
// it the per-role sandbox.RoutePolicy) the same way internal/workflow's
// TestRunWorkflowInputCannotCarryACredential walks RunWorkflowInput: a
// future typed RunSpec is meant to absorb Selection unchanged, and a
// RunSpec is persisted (run record, Temporal input), so nothing reachable
// from Selection may be, or become, a credential value.
func TestSelectionCannotCarryACredential(t *testing.T) {
	t.Parallel()
	forbiddenTypes := map[reflect.Type]bool{
		reflect.TypeOf(sandbox.RouteSecret{}): true,
		reflect.TypeOf(sandbox.RouteSpec{}):   true,
	}
	exempt := map[string]reflect.Kind{
		// Policy fields named after "credential" that hold a mode name,
		// an env var NAME, or an opt-in flag, never a value.
		"Selection.Route.CredentialMode":    reflect.String,
		"Selection.Route.CredentialEnv":     reflect.String,
		"Selection.Route.CredentialHeader":  reflect.String,
		"Selection.Route.AllowNoCredential": reflect.Bool,
		// The operator's worker models.json extras: rendered into
		// RoutePolicy.WorkerModelExtraJSON and handed to the untrusted
		// worker, so it is worker-visible by construction.
		"Selection.Model.ExtraJSON[]": reflect.Interface,
	}
	if err := testfixture.FindCredentialLeak(reflect.TypeOf(Selection{}), forbiddenTypes, exempt); err != nil {
		t.Fatal(err)
	}
}

// TestSelectionRoundTripsThroughJSON pins Selection as a plain value: a
// real SelectRoute result survives json.Marshal/Unmarshal unchanged, so no
// func, channel or unexported state rides along that a persisted RunSpec
// would silently drop.
func TestSelectionRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	s := routesModeSettings()
	luna := s.Models["luna"]
	luna.ExtraJSON = map[string]any{"contextWindow": "400000"}
	s.Models["luna"] = luna

	sel, err := SelectRoute(s, RoleExecution, "", "", "fallback", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	data, err := json.Marshal(sel)
	if err != nil {
		t.Fatalf("json.Marshal(Selection): %v", err)
	}
	var back Selection
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("json.Unmarshal(Selection): %v", err)
	}
	if !reflect.DeepEqual(back, sel) {
		t.Fatalf("Selection changed across a JSON round trip:\n got  %+v\n want %+v", back, sel)
	}
}
