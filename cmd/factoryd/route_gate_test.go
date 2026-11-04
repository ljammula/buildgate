package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/unresolved"
)

// routeGateSettings is one static route whose key is the variable env, one
// model on it, and the execution and planning roles on that model (review
// too when withReview).
func routeGateSettings(env string, withReview bool) sessionconfig.Settings {
	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{
		"keyed": {CredentialMode: "static", Upstream: "https://model.example.invalid", CredentialEnv: env},
	}
	s.Models = map[string]sessionconfig.Model{"m": {ID: "m-1", Routes: []string{"keyed"}}}
	s.Roles = &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "m"},
		Planning:  &sessionconfig.RoleConfig{Model: "m"},
	}
	if withReview {
		s.Roles.Review = &sessionconfig.RoleConfig{Model: "m", AllowSharedModel: true}
	}
	return s
}

// TestRouteWaitReasonNamesTheRouteItsCauseAndTheRestart proves a role whose
// route has no usable static key is named with the role, the route, the
// resolver's own cause and the restart that picks a new key up, and that a
// usable key gives no reason.
func TestRouteWaitReasonNamesTheRouteItsCauseAndTheRestart(t *testing.T) {
	s := routeGateSettings("RG_TEST_KEY", true)
	for _, role := range []string{"planning", "execution", "review"} {
		need := requestdriver.RouteNeed{Role: role}
		t.Setenv("RG_TEST_KEY", "")
		got := routeWaitReason(t.TempDir(), s, need, time.Now())
		for _, want := range []string{"roles." + role, "route keyed", "RG_TEST_KEY", "factoryd restart"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s with no key: reason = %q, want %q in it", role, got, want)
			}
		}
		t.Setenv("RG_TEST_KEY", "sk-test")
		if got := routeWaitReason(t.TempDir(), s, need, time.Now()); got != "" {
			t.Fatalf("%s with a key: reason = %q, want none", role, got)
		}
	}
}

// TestRouteWaitReasonFallsBackFromAnUnsetPlanningRole proves a drafting job's
// planning role, when unset, is checked as execution, as the job runs.
func TestRouteWaitReasonFallsBackFromAnUnsetPlanningRole(t *testing.T) {
	t.Setenv("RG_FALLBACK_KEY", "")
	s := routeGateSettings("RG_FALLBACK_KEY", false)
	s.Roles.Planning = nil
	if got := routeWaitReason(t.TempDir(), s, requestdriver.RouteNeed{Role: "planning", Model: "not-a-model"}, time.Now()); !strings.Contains(got, "roles.execution") {
		t.Fatalf("reason = %q, want roles.execution checked with the planning pick dropped", got)
	}
}

// TestRouteWaitReasonIsSilentWithoutRoutes proves an offline config (no
// routes:) never waits.
func TestRouteWaitReasonIsSilentWithoutRoutes(t *testing.T) {
	if got := routeWaitReason(t.TempDir(), sessionconfig.DefaultSettings(), requestdriver.RouteNeed{Role: "execution"}, time.Now()); got != "" {
		t.Fatalf("reason = %q, want none without routes:", got)
	}
}

// TestCredentialFromWorkerEnvSaysWhenARestartIsNeeded proves the wait names a
// restart only for a key read from the worker's own environment: a static
// key, or a github-copilot route with no token file. A credential read from a
// file is read again on every check.
func TestCredentialFromWorkerEnvSaysWhenARestartIsNeeded(t *testing.T) {
	cases := []struct {
		route sessionconfig.Route
		want  bool
	}{
		{sessionconfig.Route{CredentialMode: "static", CredentialEnv: "K"}, true},
		{sessionconfig.Route{CredentialMode: "github-copilot"}, true},
		{sessionconfig.Route{CredentialMode: "github-copilot", GitHubTokenFile: "~/.pi/agent/auth.json"}, false},
		{sessionconfig.Route{CredentialMode: "chatgpt-codex", CodexAuthFile: "~/.codex/auth.json"}, false},
	}
	for _, c := range cases {
		if got := credentialFromWorkerEnv(c.route); got != c.want {
			t.Errorf("credentialFromWorkerEnv(%+v) = %v, want %v", c.route, got, c.want)
		}
	}
}

// TestAdvanceRequestWaitsForARouteThenRunsTheStep proves, through the worker's
// activity and the real Deps, that a drafting job whose route has no usable
// credential does not start: the request keeps its state with the wait, the
// step is paced by routeWaitRecheck, and once the credential is usable the
// next pass ends the wait and runs the job.
func TestAdvanceRequestWaitsForARouteThenRunsTheStep(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-route", request.StateSpecDrafting)
	if err := request.SaveText(dataDir, "req-route", "text"); err != nil {
		t.Fatal(err)
	}
	ran := 0
	acts := &requestActivities{dp: newTestDeps(t), dataDir: dataDir,
		cfg: requestdriver.WorkerConfig{Settings: routeGateSettings("RG_STEP_KEY", false)},
		specRunner: func(context.Context, string, *request.Request, requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
			ran++
			return "", nil, errors.New("stop after the step started")
		},
	}

	t.Setenv("RG_STEP_KEY", "")
	after, err := acts.AdvanceRequest(context.Background(), "req-route")
	if err != nil || after != string(request.StateSpecDrafting) {
		t.Fatalf("AdvanceRequest without a key = %q, %v; want spec_drafting and no error", after, err)
	}
	if ran != 0 {
		t.Fatal("the drafting job ran while its route had no credential")
	}
	r, err := request.Load(dataDir, "req-route")
	if err != nil {
		t.Fatal(err)
	}
	if r.RouteWait == nil || !strings.Contains(r.RouteWait.On, "RG_STEP_KEY") {
		t.Fatalf("RouteWait = %+v, want the wait recorded with its cause", r.RouteWait)
	}
	if w := request.WaitFor(r, request.WaitFacts{WorkerLive: true}); w == nil || w.Kind != request.WaitRoute {
		t.Fatalf("WaitFor = %+v, want a route wait", w)
	}
	if !strings.Contains(r.NextAction(), "RG_STEP_KEY") {
		t.Fatalf("NextAction = %q, want the route wait's sentence", r.NextAction())
	}
	step, err := acts.LoadRequestStep(context.Background(), "req-route")
	if err != nil || step.Wait != routeWaitRecheck {
		t.Fatalf("LoadRequestStep = %+v, %v; want the step paced by routeWaitRecheck", step, err)
	}

	t.Setenv("RG_STEP_KEY", "sk-test")
	_, _ = acts.AdvanceRequest(context.Background(), "req-route")
	if ran != 1 {
		t.Fatalf("the drafting job ran %d times once the key was set, want 1", ran)
	}
	if r, _ = request.Load(dataDir, "req-route"); r.RouteWait != nil {
		t.Fatalf("RouteWait = %+v after the step started, want it cleared", r.RouteWait)
	}
}

// TestRouteWaitReasonWaitsWhileAQuotaRefusalHoldsTheRoute proves a launch on a
// route a request of the data dir was refused on by the provider's limit
// waits, naming that request and its resume, and launches once the holder
// leaves resume_review.
func TestRouteWaitReasonWaitsWhileAQuotaRefusalHoldsTheRoute(t *testing.T) {
	t.Setenv("RG_HOLD_KEY", "sk-test")
	s := routeGateSettings("RG_HOLD_KEY", false)
	dataDir := t.TempDir()
	holder := request.New("req-refused", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	holder.State = request.StateBuilding
	if err := holder.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.ProviderQuota, Routes: []string{"keyed"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := holder.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	need := requestdriver.RouteNeed{Role: "execution"}
	got := routeWaitReason(dataDir, s, need, time.Now())
	for _, want := range []string{"route keyed", "req-refused", "`factoryd resume req-refused`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reason = %q, want %q in it", got, want)
		}
	}
	resumed := requestdriver.RouteNeed{Role: "execution", ResumedAt: time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)}
	if got := routeWaitReason(dataDir, s, resumed, time.Now()); got != "" {
		t.Fatalf("reason for a request resumed after the refusal = %q, want none: its resume says the limit has reset", got)
	}
	if err := holder.Cancel("alice", "done with it", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := holder.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if got := routeWaitReason(dataDir, s, need, time.Now()); got != "" {
		t.Fatalf("reason after the holder was cancelled = %q, want none", got)
	}
}
