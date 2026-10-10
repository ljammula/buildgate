package main

import (
	"path/filepath"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// TestRequestCommandsHonourConfigFlagDataDir proves approve, reject,
// cancel, retry and watch read data_dir from an explicit -config file, not
// from the default session config path: an operator running a second
// config (e.g. a per-project one) could submit through it but not act on
// the request it created.
func TestRequestCommandsHonourConfigFlagDataDir(t *testing.T) {
	dp := newTestDeps(t)
	cases := []struct {
		name      string
		run       func(configPath string) error
		wantState request.State
	}{
		{"approve", func(c string) error { return approveMain(dp, []string{"-config", c, "req-1"}) }, request.StatePlanning},
		{"reject", func(c string) error { return rejectMain(dp, []string{"-config", c, "-reason", "redo", "req-1"}) }, request.StateSpecDrafting},
		{"cancel", func(c string) error { return cancelMain(dp, []string{"-config", c, "req-1"}) }, request.StateCancelled},
		{"watch", func(c string) error { return watchMain([]string{"-config", c, "-no-follow", "req-1"}) }, request.StateSpecReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaultConfig := isolateSessionConfig(t)
			writeDataDirSessionConfig(t, defaultConfig, t.TempDir())
			dataDir := t.TempDir()
			explicitConfig := filepath.Join(t.TempDir(), "other.yml")
			writeDataDirSessionConfig(t, explicitConfig, dataDir)
			requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)

			if err := tc.run(explicitConfig); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			loaded, err := request.Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != tc.wantState {
				t.Errorf("State = %q, want %q", loaded.State, tc.wantState)
			}
		})
	}

	t.Run("retry", func(t *testing.T) {
		defaultConfig := isolateSessionConfig(t)
		writeDataDirSessionConfig(t, defaultConfig, t.TempDir())
		dataDir := t.TempDir()
		explicitConfig := filepath.Join(t.TempDir(), "other.yml")
		writeDataDirSessionConfig(t, explicitConfig, dataDir)
		saveRetryTestRequest(t, dataDir, "t1", request.StateHalted)

		if err := retryMain(dp, []string{"-config", explicitConfig, "t1"}); err != nil {
			t.Fatalf("retryMain: %v", err)
		}
		got, err := request.Load(dataDir, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.State == request.StateHalted {
			t.Errorf("state = %q, want it moved out of halted", got.State)
		}
	})
}
