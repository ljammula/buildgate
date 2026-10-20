package sandbox

import (
	"context"
	"errors"
	"testing"

	"buildgate/internal/composeservices"
)

func volumesLifecycle(t *testing.T, hooks ComposeServicesHooks) *ComposeServicesLifecycle {
	t.Helper()
	spec := ComposeServicesSpec{ComposeYAML: []byte("services:\n  db:\n    image: docker.io/library/postgres:16\n"),
		ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", t.TempDir(), hooks)
	if err != nil || l.Disabled() {
		t.Fatalf("BeginComposeServicesLifecycle: %v (disabled %v)", err, l.Disabled())
	}
	return l
}

func TestAComposeProjectsVolumesAreConfirmedGoneOrNot(t *testing.T) {
	ctx := context.Background()
	if !(*ComposeServicesLifecycle)(nil).VolumesConfirmedGone() {
		t.Error("a nil lifecycle has volumes to leave")
	}
	for name, tc := range map[string]struct {
		present bool
		err     error
		want    bool
	}{
		"removed":           {false, nil, true},
		"left behind":       {true, nil, false},
		"the list failed":   {false, errors.New("daemon busy"), false},
		"left and list err": {true, errors.New("x"), false},
	} {
		hooks := noopHooks()
		hooks.ProjectVolumesPresent = func(_ context.Context, _, project string) (bool, error) {
			if project != "bg-run-1-a1" {
				t.Errorf("%s: volumes listed for project %q", name, project)
			}
			return tc.present, tc.err
		}
		l := volumesLifecycle(t, hooks)
		if !l.VolumesConfirmedGone() {
			t.Errorf("%s: a lifecycle that tore nothing down reports volumes left", name)
		}
		if err := l.EnsureForAttempt(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if err := l.TeardownAttempt(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if got := l.VolumesConfirmedGone(); got != tc.want {
			t.Errorf("%s after TeardownAttempt: VolumesConfirmedGone = %v, want %v", name, got, tc.want)
		}
	}
	// Cleanup confirms again, and the last answer for a project stands.
	hooks := noopHooks()
	left := true
	hooks.ProjectVolumesPresent = func(context.Context, string, string) (bool, error) { return left, nil }
	l := volumesLifecycle(t, hooks)
	if err := l.TeardownAttempt(ctx, 1); err != nil {
		t.Fatal(err)
	}
	left = false
	if err := l.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if !l.VolumesConfirmedGone() {
		t.Error("volumes removed by the backstop Cleanup still read as left")
	}
}

func TestAReleaseIsNotReadWhileSidecarVolumesRemain(t *testing.T) {
	rt := &workerRuntime{disk: []uint64{50 * gib}}
	r := Resources{DiskKnown: true, DiskFree0: 50 * gib, DiskFree1: gib, SidecarVolumesLeft: true}
	r.ReadReleased(context.Background(), rt)
	if r.ReleasedKnown || r.NotFilledByItself() {
		t.Errorf("a release was read while compose volumes remained: %+v", r)
	}
	r.SidecarVolumesLeft = false
	r.ReadReleased(context.Background(), rt)
	if !r.ReleasedKnown {
		t.Errorf("no release read once the volumes are gone: %+v", r)
	}
}
