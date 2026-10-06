package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/imageinputs"
)

// writeFakeDockerLabels writes a fake `docker` executable whose `image
// inspect --format '{{ index .Config.Labels "<name>" }}'` answers kind
// for the buildgate.image label and inputsHash for
// buildgate.inputs-hash, printing "<no value>" for either left
// empty (matching a real Go template's own behavior for a missing map
// key) -- the same fake-docker-binary convention
// project_check_and_doctor_test.go's own doctorCheckImagePresent tests
// use. The requested label name is matched inside the fixed --format
// argument this package's own imageLabelFor always passes as one argv
// item ($4 here -- no shell re-splitting happens for an exec.Command
// argv slice), so this one fake binary answers both labels correctly
// regardless of which one a given call asks for.
func writeFakeDockerLabels(t *testing.T, kind, inputsHash string) string {
	return writeFakeDockerLabelsWithLegacy(t, kind, inputsHash, "", "")
}

// writeFakeDockerLabelsWithLegacy also answers the pre-rename
// software-factory.* keys with legacyKind/legacyHash ("" prints "<no value>").
func writeFakeDockerLabelsWithLegacy(t *testing.T, kind, inputsHash, legacyKind, legacyHash string) string {
	t.Helper()
	printLegacyKind, printLegacyHash := legacyKind, legacyHash
	if printLegacyKind == "" {
		printLegacyKind = "<no value>"
	}
	if printLegacyHash == "" {
		printLegacyHash = "<no value>"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-docker")
	printKind := kind
	if printKind == "" {
		printKind = "<no value>"
	}
	printHash := inputsHash
	if printHash == "" {
		printHash = "<no value>"
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = image ] && [ \"$2\" = inspect ]; then\n" +
		"  case \"$4\" in\n" +
		"    *" + legacyImageKindLabel + "*) echo '" + printLegacyKind + "'; exit 0 ;;\n" +
		"    *" + legacyImageInputsHashLabel + "*) echo '" + printLegacyHash + "'; exit 0 ;;\n" +
		"    *" + imageKindLabel + "*) echo '" + printKind + "'; exit 0 ;;\n" +
		"    *" + imageInputsHashLabel + "*) echo '" + printHash + "'; exit 0 ;;\n" +
		"  esac\n" +
		"fi\n" +
		"echo \"unexpected: $@\" >&2; exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return path
}

func TestImageInputsHashForReadsLabel(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabels(t, "worker", "deadbeef")
	got, err := imageInputsHashFor(context.Background(), docker, "some-image")
	if err != nil {
		t.Fatalf("imageInputsHashFor: %v", err)
	}
	if got != "deadbeef" {
		t.Errorf("got %q, want %q", got, "deadbeef")
	}
}

func TestImageInputsHashForNoLabelIsEmpty(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabels(t, "worker", "")
	got, err := imageInputsHashFor(context.Background(), docker, "some-image")
	if err != nil {
		t.Fatalf("imageInputsHashFor: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (unlabeled)", got)
	}
}

func TestImageKindForReadsLabel(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabels(t, "pifork", "deadbeef")
	got, err := imageKindFor(context.Background(), docker, "some-image")
	if err != nil {
		t.Fatalf("imageKindFor: %v", err)
	}
	if got != "pifork" {
		t.Errorf("got %q, want %q", got, "pifork")
	}
}

// writeStaleFixtureRepo lays out a minimal repo whose imageinputs.Hash
// for "worker"/"pifork" is deterministic, for the fresh/stale
// comparison tests.
func writeStaleFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                      "module fixture\n",
		"go.sum":                      "",
		"internal/sandbox/Dockerfile": "FROM golang\n",
		"cmd/bg-forward/main.go":      "package main\n",
	}
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestImageStalenessFresh(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	current, err := imageinputs.Hash(root, "worker")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	docker := writeFakeDockerLabels(t, "worker", current)
	stale, verified, kind := imageStaleness(context.Background(), docker, "some-image", root)
	if !verified {
		t.Fatal("verified = false, want true (label and root both present)")
	}
	if stale {
		t.Error("stale = true, want false: label matches the current source hash")
	}
	if kind != "worker" {
		t.Errorf("kind = %q, want worker", kind)
	}
}

func TestImageStalenessStale(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, "worker", "not-the-real-hash")
	stale, verified, _ := imageStaleness(context.Background(), docker, "some-image", root)
	if !verified {
		t.Fatal("verified = false, want true")
	}
	if !stale {
		t.Error("stale = false, want true: label does not match the current source hash")
	}
}

// TestImageStalenessHashesTheLabeledKindNotAGuess is the regression test
// for an adversarial review of the ghcr-removal change: a
// pifork-labeled image must be hashed against imageinputs.Hash(root,
// "pifork"), never against "worker" -- Docker inherits labels through
// FROM, so a naive guess based on config key or a harness would silently
// mis-hash it (and, for a project image, would falsely flag it stale and
// offer to rebuild it as a plain worker, discarding its own baked
// dependencies).
func TestImageStalenessHashesTheLabeledKindNotAGuess(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	piforkHash, err := imageinputs.Hash(root, "pifork")
	if err != nil {
		t.Fatalf("Hash(pifork): %v", err)
	}
	workerHash, err := imageinputs.Hash(root, "worker")
	if err != nil {
		t.Fatalf("Hash(worker): %v", err)
	}
	if piforkHash == workerHash {
		t.Fatal("fixture bug: pifork and worker hashes must differ for this test to prove anything")
	}
	// Labeled pifork, and actually stamped with the real pifork hash --
	// must report fresh, not stale-against-worker.
	docker := writeFakeDockerLabels(t, "pifork", piforkHash)
	stale, verified, kind := imageStaleness(context.Background(), docker, "some-image", root)
	if !verified || stale {
		t.Errorf("stale=%v verified=%v, want verified=true stale=false: a pifork image stamped with its own real hash must not be hashed as worker", stale, verified)
	}
	if kind != "pifork" {
		t.Errorf("kind = %q, want pifork", kind)
	}
}

// TestImageStalenessProjectKindIsNeverVerified is the regression test for
// another adversarial-review finding: a project-sandbox-image build
// inherits the worker's own buildgate.image label through FROM unless
// project-sandbox-image overrides it -- once overridden to "project",
// staleness must never be computed for it (there is no in-repo source
// to hash a project image against), regardless of what inputs-hash
// label it happens to carry.
func TestImageStalenessProjectKindIsNeverVerified(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, projectImageKind, "some-hash-that-would-never-match")
	stale, verified, kind := imageStaleness(context.Background(), docker, "some-image", root)
	if verified {
		t.Error("verified = true, want false: a project image must never be verified for staleness")
	}
	if stale {
		t.Error("stale = true, want false: an unverified result must not also claim staleness")
	}
	if kind != projectImageKind {
		t.Errorf("kind = %q, want %q", kind, projectImageKind)
	}
}

// TestImageStalenessDetectsStaleBaseImage is the regression test for a
// real finding from adversarial review, 2026-09-25 (Round 2 of the
// ghcr-removal change): pifork's build-time hash used to always
// fold in the CURRENT checkout's own worker Hash, so an image built
// against an old, no-longer-current BASE_IMAGE (imageinputs.HashWithBase's
// whole reason to exist -- see that function's own doc comment) still got
// stamped with a hash that matched today's checkout, silently reporting
// fresh. Simulates that exact build: stamp the image with the hash
// HashWithBase would have computed against an old base hash, then prove
// imageStaleness -- which always recomputes against imageinputs.Hash (the
// CURRENT checkout's own worker hash) at check time, deliberately -- flags
// it stale.
func TestImageStalenessDetectsStaleBaseImage(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	staleBuildTimeHash, err := imageinputs.HashWithBase(root, "pifork", "an-old-base-images-inputs-hash")
	if err != nil {
		t.Fatalf("HashWithBase: %v", err)
	}
	currentHash, err := imageinputs.Hash(root, "pifork")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if staleBuildTimeHash == currentHash {
		t.Fatal("fixture bug: the old-base build-time hash must differ from the current checkout's own hash for this test to prove anything")
	}
	docker := writeFakeDockerLabels(t, "pifork", staleBuildTimeHash)
	stale, verified, kind := imageStaleness(context.Background(), docker, "some-image", root)
	if !verified {
		t.Fatal("verified = false, want true")
	}
	if !stale {
		t.Error("stale = false, want true: the image was stamped against an old BASE_IMAGE's own inputs-hash, which no longer matches the current checkout's own worker hash")
	}
	if kind != "pifork" {
		t.Errorf("kind = %q, want pifork", kind)
	}
}

func TestImageStalenessUnverifiableCases(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	t.Run("no source root", func(t *testing.T) {
		docker := writeFakeDockerLabels(t, "worker", "irrelevant")
		if _, verified, _ := imageStaleness(context.Background(), docker, "some-image", ""); verified {
			t.Error("verified = true, want false: no source root configured")
		}
	})
	t.Run("no kind label", func(t *testing.T) {
		docker := writeFakeDockerLabels(t, "", "deadbeef")
		if _, verified, kind := imageStaleness(context.Background(), docker, "some-image", root); verified || kind != "" {
			t.Errorf("verified=%v kind=%q, want verified=false kind=\"\": a missing kind label must be cannot-verify", verified, kind)
		}
	})
	t.Run("unlabeled inputs-hash", func(t *testing.T) {
		docker := writeFakeDockerLabels(t, "worker", "")
		if _, verified, _ := imageStaleness(context.Background(), docker, "some-image", root); verified {
			t.Error("verified = true, want false: image was never labeled with an inputs-hash")
		}
	})
	t.Run("docker failure", func(t *testing.T) {
		docker := filepath.Join(t.TempDir(), "does-not-exist")
		if _, verified, _ := imageStaleness(context.Background(), docker, "some-image", root); verified {
			t.Error("verified = true, want false: docker itself is unreachable")
		}
	})
	t.Run("unreadable source root", func(t *testing.T) {
		docker := writeFakeDockerLabels(t, "worker", "deadbeef")
		if _, verified, _ := imageStaleness(context.Background(), docker, "some-image", filepath.Join(t.TempDir(), "does-not-exist")); verified {
			t.Error("verified = true, want false: source root does not exist")
		}
	})
}

func TestDoctorCheckImageStaleReportsAdvisoryOnStale(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, "worker", "not-the-real-hash")
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", root)
	if !ok {
		t.Fatal("ok = false, want true: staleness was determined")
	}
	if check.Err == nil {
		t.Fatal("Err = nil, want a stale-image error")
	}
	if !check.Advisory {
		t.Error("Advisory = false, want true: staleness must never fail the overall doctor run")
	}
	if check.Fix == "" {
		t.Error("Fix is empty, want the make-install-there guidance")
	}
}

// A stale pifork image is rebuilt by its own target: make install would
// build only the plain worker image.
func TestDoctorCheckImageStaleNamesThePiforkTarget(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, "pifork", "not-the-real-hash")
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", root)
	if !ok || check.Err == nil {
		t.Fatalf("check = %+v, ok = %v, want a stale-image advisory", check, ok)
	}
	if !strings.Contains(check.Fix, "make pifork-image") || strings.Contains(check.Fix, "make install") {
		t.Errorf("Fix = %q, want it to name make pifork-image, not make install", check.Fix)
	}
}

func TestDoctorCheckImageStalePassesWhenFresh(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	current, err := imageinputs.Hash(root, "worker")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	docker := writeFakeDockerLabels(t, "worker", current)
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", root)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if check.Err != nil {
		t.Errorf("Err = %v, want nil (fresh)", check.Err)
	}
}

func TestDoctorCheckImageStaleSkippedWhenUnverifiable(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabels(t, "worker", "")
	root := writeStaleFixtureRepo(t)
	_, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", root)
	if ok {
		t.Error("ok = true, want false: an unlabeled image must not produce a check at all (never a false pass)")
	}
}

// TestDoctorCheckImageStaleReportsProjectImageAsUnverifiable is the
// regression test for a related adversarial-review finding: a
// project-kind image must produce a visible, informational check
// (never silently skipped, and never a real failure) whose name does
// NOT match quickstartImageStaleChecks'
// own "up to date with" filter -- so quickstart's stale-rebuild offer
// (which runs `make local-images`, rebuilding the plain worker/relay/
// registry-proxy images, never a project one) never fires for it.
func TestDoctorCheckImageStaleReportsProjectImageAsUnverifiable(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, projectImageKind, "irrelevant")
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", root)
	if !ok {
		t.Fatal("ok = false, want true: a project image must still produce a visible check")
	}
	if check.Err == nil {
		t.Fatal("Err = nil, want the project-image unverifiable message")
	}
	if !check.Advisory {
		t.Error("Advisory = false, want true: a project image's unverifiable status must never fail the overall doctor run")
	}
	if quickstartImageStaleChecksMatches(check) {
		t.Error("this check's Name matches quickstartImageStaleChecks' own filter -- quickstart would wrongly offer to rebuild a project image via make local-images")
	}
}

// TestImageStalenessRetiredCopilotKindIsNeverVerified is
// TestImageStalenessProjectKindIsNeverVerified's own sibling for
// retiredImageKind (review round 1): imageinputs.Hash no longer accepts
// "copilot" at all (its imagePaths entry was removed alongside the
// engine), so staleness must never even attempt to compute for it,
// regardless of what inputs-hash label it happens to carry.
func TestImageStalenessRetiredCopilotKindIsNeverVerified(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, retiredImageKind, "some-hash-that-would-never-match")
	stale, verified, kind := imageStaleness(context.Background(), docker, "some-image", root)
	if verified {
		t.Error("verified = true, want false: a retired-engine image must never be verified for staleness")
	}
	if stale {
		t.Error("stale = true, want false: an unverified result must not also claim staleness")
	}
	if kind != retiredImageKind {
		t.Errorf("kind = %q, want %q", kind, retiredImageKind)
	}
}

// TestDoctorCheckImageStaleReportsRetiredCopilotImageAsInformational is
// TestDoctorCheckImageStaleReportsProjectImageAsUnverifiable's own
// sibling for retiredImageKind: a leftover buildgate.image=copilot
// sandbox image must produce a visible, informational row (never
// silently skipped the way an absent/unlabeled image is) naming what it
// is and how to replace it, and must not match quickstartImageStaleChecks'
// own "up to date with" filter -- quickstart's stale-rebuild offer must
// never try to run `make copilot-image` (gone) for it.
func TestDoctorCheckImageStaleReportsRetiredCopilotImageAsInformational(t *testing.T) {
	t.Parallel()
	root := writeStaleFixtureRepo(t)
	docker := writeFakeDockerLabels(t, retiredImageKind, "irrelevant")
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", root)
	if !ok {
		t.Fatal("ok = false, want true: a retired-engine image must still produce a visible check")
	}
	if check.Err == nil || !strings.Contains(check.Err.Error(), "retired copilot") {
		t.Fatalf("Err = %v, want it to name the retired copilot engine", check.Err)
	}
	if !check.Advisory {
		t.Error("Advisory = false, want true: a retired-engine image's informational row must never fail the overall doctor run")
	}
	if quickstartImageStaleChecksMatches(check) {
		t.Error("this check's Name matches quickstartImageStaleChecks' own filter -- quickstart would wrongly offer to rebuild a retired copilot image via make local-images")
	}
}

// TestDoctorCheckImageStaleReportsRetiredCopilotImageWithoutSourceRoot is
// the regression test for a real finding from review round 2: an
// operator with no image_source_root configured at all (e.g. one who has
// never run `make install`/`factoryd configure-images`) used to lose the
// retired-copilot informational row entirely -- imageStaleness's own
// image == "" || sourceRoot == "" early return skipped reading the
// image's own kind label before ever checking sourceRoot, so
// doctorCheckImageStale's kind == retiredImageKind branch never saw a
// populated kind to match against. The row must show regardless of
// whether a source root is configured, exactly like a project image's
// own row already did (TestDoctorCheckImageStaleReportsProjectImageAsUnverifiable
// is this test's own project-kind counterpart, which passed before this
// fix only because it happened to pass a real root anyway).
func TestDoctorCheckImageStaleReportsRetiredCopilotImageWithoutSourceRoot(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabels(t, retiredImageKind, "irrelevant")
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", "")
	if !ok {
		t.Fatal("ok = false, want true: a retired-engine image must still produce a visible check even with no image_source_root configured")
	}
	if check.Err == nil || !strings.Contains(check.Err.Error(), "retired copilot") {
		t.Fatalf("Err = %v, want it to name the retired copilot engine", check.Err)
	}
	if !check.Advisory {
		t.Error("Advisory = false, want true")
	}
}

// TestDoctorCheckImageStaleReportsProjectImageWithoutSourceRoot is
// TestDoctorCheckImageStaleReportsRetiredCopilotImageWithoutSourceRoot's
// own sibling for projectImageKind -- same real finding, same fix
// (imageStaleness reads the image's own kind label before ever checking
// sourceRoot == "").
func TestDoctorCheckImageStaleReportsProjectImageWithoutSourceRoot(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabels(t, projectImageKind, "irrelevant")
	check, ok := doctorCheckImageStale(context.Background(), docker, "sandbox image", "some-image", "")
	if !ok {
		t.Fatal("ok = false, want true: a project image must still produce a visible check even with no image_source_root configured")
	}
	if check.Err == nil {
		t.Fatal("Err = nil, want the project-image unverifiable message")
	}
	if !check.Advisory {
		t.Error("Advisory = false, want true")
	}
}

// quickstartImageStaleChecksMatches mirrors quickstartImageStaleChecks'
// own per-check predicate, for a test that needs to check just one
// doctorCheck rather than filter a slice.
func quickstartImageStaleChecksMatches(c doctorCheck) bool {
	got := quickstartImageStaleChecks([]doctorCheck{c})
	return len(got) == 1
}

// An image built before the rename carries only software-factory.* labels;
// it must still be read (and judged stale by hash value), not reported
// unbuilt/unverifiable because of the rename.
func TestImageLabelsFallBackToLegacyKeys(t *testing.T) {
	t.Parallel()
	docker := writeFakeDockerLabelsWithLegacy(t, "", "", "worker", "oldhash")
	if got, err := imageKindFor(context.Background(), docker, "img"); err != nil || got != "worker" {
		t.Errorf("imageKindFor = %q, %v; want worker from the legacy key", got, err)
	}
	if got, err := imageInputsHashFor(context.Background(), docker, "img"); err != nil || got != "oldhash" {
		t.Errorf("imageInputsHashFor = %q, %v; want oldhash from the legacy key", got, err)
	}
	// The current key wins when both are present.
	both := writeFakeDockerLabelsWithLegacy(t, "worker", "newhash", "relay", "oldhash")
	if got, _ := imageInputsHashFor(context.Background(), both, "img"); got != "newhash" {
		t.Errorf("imageInputsHashFor = %q, want the current key's newhash", got)
	}
}
