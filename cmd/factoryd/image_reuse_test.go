package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeDockerReuse writes a fake `docker` executable answering the
// buildgate.inputs-hash label query imageReuseDecision makes against ref
// (existingHash, "<no value>" when empty, matching a real Go template's own
// behavior for a missing map key -- see writeFakeDockerLabels's own doc
// comment).
func writeFakeDockerReuse(t *testing.T, existingHash string) string {
	t.Helper()
	printHash := existingHash
	if printHash == "" {
		printHash = "<no value>"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = image ] && [ \"$2\" = inspect ]; then\n" +
		"  case \"$4\" in\n" +
		"    *" + imageInputsHashLabel + "*) echo '" + printHash + "'; exit 0 ;;\n" +
		"  esac\n" +
		"fi\n" +
		"echo \"unexpected: $@\" >&2; exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return path
}

func TestImageReuseDecisionMatchingHashReuses(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerReuse(t, "samehash")
	if !imageReuseDecision(context.Background(), docker, "worker", "localhost:5050/buildgate-worker:local", "samehash", false) {
		t.Fatal("reuse = false, want true: ref's stamped label already matches the fresh hash")
	}
}

func TestImageReuseDecisionDifferentHashBuilds(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerReuse(t, "oldhash")
	if imageReuseDecision(context.Background(), docker, "worker", "localhost:5050/buildgate-worker:local", "newhash", false) {
		t.Fatal("reuse = true, want false: ref's stamped label is stale")
	}
}

func TestImageReuseDecisionMissingLabelBuilds(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerReuse(t, "")
	if imageReuseDecision(context.Background(), docker, "worker", "localhost:5050/buildgate-worker:local", "freshhash", false) {
		t.Fatal("reuse = true, want false: ref was never labeled (or never built)")
	}
}

func TestImageReuseDecisionForceAlwaysBuilds(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerReuse(t, "samehash")
	if imageReuseDecision(context.Background(), docker, "worker", "localhost:5050/buildgate-worker:local", "samehash", true) {
		t.Fatal("reuse = true, want false: -force must always report 'must build'")
	}
}

func TestImageReuseDecisionPiforkNeverReused(t *testing.T) {
	t.Parallel()
	// pifork's inputs hash ignores its build-time version args (see
	// imageReuseCandidateKinds' own doc comment) -- reuse must never be
	// offered for it even when the label matches.
	docker := writeFakeDockerReuse(t, "samehash")
	for _, kind := range []string{"pifork"} {
		if imageReuseDecision(context.Background(), docker, kind, "localhost:5050/buildgate-"+kind+":local", "samehash", false) {
			t.Errorf("%s: reuse = true, want false: version build args aren't in the inputs hash", kind)
		}
	}
}

func TestImageReuseDecisionEmptyFreshHashBuilds(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerReuse(t, "samehash")
	if imageReuseDecision(context.Background(), docker, "worker", "localhost:5050/buildgate-worker:local", "", false) {
		t.Fatal("reuse = true, want false: an empty freshHash must never match")
	}
}
