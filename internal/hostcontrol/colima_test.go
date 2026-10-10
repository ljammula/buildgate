package hostcontrol

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol/hostcontroltest"
)

// newColimaFake points dp at a fake docker and colima on disk
// (hostcontroltest.NewColima) for the length of the test.
func newColimaFake(dp *fakeDeps, t *testing.T, ctxName string, infoDown bool, ps ...string) *hostcontroltest.Colima {
	t.Helper()
	f := hostcontroltest.NewColima(t, ctxName, infoDown, ps...)
	prevD, prevC := dp.DockerBinary(), dp.ColimaBinary()
	dp.dockerBinaryFn, dp.colimaBinaryFn = f.DockerBinary, f.ColimaBinary
	t.Cleanup(func() {
		dp.dockerBinaryFn, dp.colimaBinaryFn = func() string { return prevD }, func() string { return prevC }
	})
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return f
}

func markerPath(t *testing.T) string {
	t.Helper()
	dir, err := TemporalStackDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, ColimaStoppedMarker)
}

func TestColimaProfileArgs(t *testing.T) {
	if got := strings.Join(ColimaArgs("stop", "default"), " "); got != "stop" {
		t.Errorf("default profile args = %q", got)
	}
	if got := strings.Join(ColimaArgs("start", "work"), " "); got != "start --profile work" {
		t.Errorf("named profile args = %q", got)
	}
}

func TestColimaProfileDetection(t *testing.T) {
	dp := newFakeDeps(t)
	cases := []struct {
		ctx     string
		profile string
		ok      bool
	}{
		{"colima", "default", true},
		{"colima-work", "work", true},
		{"default", "", false},
		{"desktop-linux", "", false},
	}
	for _, c := range cases {
		newColimaFake(dp, t, c.ctx, false)
		profile, ok := ColimaProfile(dp, context.Background())
		if profile != c.profile || ok != c.ok {
			t.Errorf("context %q: got (%q, %v), want (%q, %v)", c.ctx, profile, ok, c.profile, c.ok)
		}
	}
}

func TestColimaProfileNeedsTheColimaBinary(t *testing.T) {
	dp := newFakeDeps(t)
	newColimaFake(dp, t, "colima", false)
	dp.colimaBinaryFn = func() string { return filepath.Join(t.TempDir(), "absent") }
	if _, ok := ColimaProfile(dp, context.Background()); ok {
		t.Error("colima context without a colima binary must not count as Colima")
	}
}

func TestAutostartStartsColimaWhenDockerIsDownThenProceeds(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima-work", true)
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(f.DockerLog)
		return strings.Contains(string(log), "up -d")
	})
	dp.dockerBinaryFn = func() string { return filepath.Join(f.Dir, "docker") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got == "" {
		t.Fatalf("startTemporal failed:\n%s", out.String())
	}
	if got := strings.TrimSpace(f.ColimaCalls()); got != "start --profile work" {
		t.Errorf("colima calls = %q, want start --profile work", got)
	}
	if !strings.Contains(out.String(), "Temporal started at") {
		t.Errorf("output lacks the Temporal line:\n%s", out.String())
	}
}

func TestAutostartReportsWhyColimaCouldNotStart(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", true)
	if err := os.WriteFile(f.StartFail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stubTemporalProbe(dp, t, func() bool { return false })
	dp.dockerBinaryFn = func() string { return filepath.Join(f.Dir, "docker") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	for _, want := range []string{"docker is not usable", "`colima start` failed", "colima-start.log"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if log, _ := os.ReadFile(f.DockerLog); strings.Contains(string(log), "compose") {
		t.Errorf("compose ran although Colima did not start:\n%s", log)
	}
}

func TestAutostartDockerDownWithoutColimaStartsNothing(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "desktop-linux", true)
	stubTemporalProbe(dp, t, func() bool { return false })
	dp.dockerBinaryFn = func() string { return filepath.Join(f.Dir, "docker") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	StartTemporal(dp, context.Background(), &out)
	if f.ColimaCalls() != "" {
		t.Errorf("colima ran although it is not the provider: %q", f.ColimaCalls())
	}
}

func TestAutostartIgnoresColimaWithoutMarkerOrContext(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	cases := map[string]string{"no marker": "", "traversal": "../x\n", "two lines": "work\nother\n", "empty": "\n"}
	for name, marker := range cases {
		t.Run(name, func(t *testing.T) {
			f := newColimaFake(dp, t, "default", true)
			stubTemporalProbe(dp, t, func() bool { return false })
			dp.dockerBinaryFn = func() string { return filepath.Join(f.Dir, "docker") }
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if marker != "" {
				if err := os.WriteFile(markerPath(t), []byte(marker), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if got := StartTemporal(dp, context.Background(), &out); got != "" {
				t.Fatalf("startTemporal = %q", got)
			}
			if f.ColimaCalls() != "" || !strings.Contains(out.String(), "docker is not usable") {
				t.Errorf("colima=%q out=%q", f.ColimaCalls(), out.String())
			}
		})
	}
}
