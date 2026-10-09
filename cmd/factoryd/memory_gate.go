package main

import (
	"fmt"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/sessionconfig"
)

// memoryGate is the first call of every memory verb that changes anything. It
// resolves the three facts the gate weighs and returns the project's opened
// store and the repository's budget, or the refusal naming what stopped it:
// the project's kill switch (read from the data dir exactly as a release
// decision does), the repository's entry in the session config, and the
// project's stop marker. The kill switch and the config switch are checked
// before the store is opened, so a refused call creates nothing on disk.
func memoryGate(dp *deps, settings sessionconfig.Settings, dataDir, repoRoot, project string) (*memory.Store, memory.Budget, error) {
	_ = dp // the gate reads the data dir and the config only; dp is the seam later verbs share
	if err := memory.ValidProject(project); err != nil {
		return nil, memory.Budget{}, err
	}
	engaged, err := release.IsEngaged(dataDir, project)
	if err != nil {
		return nil, memory.Budget{}, fmt.Errorf("memory: read the kill switch for project %q: %w", project, err)
	}
	budget, on := settings.MemoryFor(repoRoot)
	if err := memory.Gate(on, false, engaged); err != nil {
		return nil, memory.Budget{}, err
	}
	store, err := memory.Open(dataDir, memory.StoreKey(project, repoRoot))
	if err != nil {
		return nil, memory.Budget{}, err
	}
	marker, err := store.Off()
	if err != nil {
		return nil, memory.Budget{}, err
	}
	if err := memory.Gate(on, marker, engaged); err != nil {
		return nil, memory.Budget{}, err
	}
	return store, budget, nil
}
