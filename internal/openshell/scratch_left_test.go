package openshell

import (
	"context"
	"errors"
	"testing"
)

func TestDeleteReportsAScratchVolumeItCouldNotRemove(t *testing.T) {
	rt, _, volumes, _, _ := scratchRuntime()
	ctx := context.Background()
	ref, err := rt.Create(ctx, scratchRequest())
	if err != nil {
		t.Fatal(err)
	}
	volumes.removeErr = errors.New("busy")
	if err := rt.Delete(ctx, ref.Name); err != nil {
		t.Fatalf("Delete: %v (a leftover volume only warns)", err)
	}
	if !rt.ScratchVolumeLeft(ref.Name) {
		t.Error("the volume Delete could not remove was not reported to the caller")
	}
	if rt.ScratchVolumeLeft(ref.Name) {
		t.Error("the answer was kept after it was read")
	}
	if rt.ScratchVolumeLeft("bg-another") {
		t.Error("another sandbox's volume reported left")
	}
	if (&Lazy{}).ScratchVolumeLeft(ref.Name) {
		t.Error("a Lazy with no connection reported a leftover volume")
	}
}
