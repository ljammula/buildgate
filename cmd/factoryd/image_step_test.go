package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/progress"
	"buildgate/internal/toolchain"
)

// TestImageStepIsMarked: the sandbox image is the run's first start-up step.
// Fitting the image to the repository writes an image stage to the run's
// feed (the id is minted before it), naming the image and whether it is the
// configured one or one derived for the repository's toolchains, and a failed
// derivation ends the stage as failed.
func TestImageStepIsMarked(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	const base = "worker@sha256:aaaa"
	run := func(t *testing.T, files map[string]string, buildErr error) (*ticketRun, []progress.Event, error) {
		t.Helper()
		dp := newTestDeps(t)
		fakeDockerOf(dp).imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
			installed, codename := parseImageToolchains(workerImageToolchains)
			return installed, codename, nil
		}
		fakeDockerOf(dp).toolImageFn = func(context.Context, string, string, []string) (string, error) {
			return "localhost:5050/buildgate-toolchain@sha256:cccc", buildErr
		}
		dataDir, image, docker, runID, ticket := t.TempDir(), base, "docker", "", "001"
		tr := &ticketRun{dp: dp, lifecycleCtx: context.Background(), resolvedWorkspace: toolchainRepo(t, files)}
		tr.dataDir, tr.sandboxImage, tr.sandboxDocker, tr.runID, tr.ticket = &dataDir, &image, &docker, &runID, &ticket
		err := tr.fitSandboxToRepository()
		events, readErr := progress.Read(progress.Path(dataDir, tr.id))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return tr, events, err
	}

	tr, events, err := run(t, map[string]string{}, nil)
	if err != nil || tr.id == "" || !strings.HasPrefix(tr.id, "001-") {
		t.Fatalf("configured image: id %q, err %v", tr.id, err)
	}
	if len(events) != 2 || events[0].Stage != "image" || events[0].Event != "start" || events[1].Event != "end" || events[1].Outcome != "pass" || events[1].Detail != base+" (configured)" {
		t.Fatalf("feed = %+v, want an image stage naming the configured image", events)
	}

	_, events, err = run(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n"}, nil)
	if err != nil || len(events) != 2 || !strings.Contains(events[1].Detail, "derived for the repository's toolchains") || !strings.Contains(events[1].Detail, "buildgate-toolchain@sha256:cccc") {
		t.Fatalf("derived image: err %v, feed %+v", err, events)
	}

	_, events, err = run(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n"}, errors.New("registry unreachable"))
	if err == nil || len(events) != 2 || events[1].Outcome != "fail" || !strings.Contains(events[1].Detail, "registry unreachable") {
		t.Fatalf("failed derivation: err %v, feed %+v", err, events)
	}
}
