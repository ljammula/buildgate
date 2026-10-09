package main

import (
	"errors"
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/sessionconfig"
)

func memorySettings(root string) sessionconfig.Settings {
	return sessionconfig.Settings{Memory: []sessionconfig.MemoryRepository{{Path: root, Budget: memory.Budget{Lines: 7, Chars: 700}}}}
}

func TestMemoryGateOpensStoreWhenAllowed(t *testing.T) {
	data, root := t.TempDir(), t.TempDir()
	store, budget, err := memoryGate(newTestDeps(t), memorySettings(root), data, root, "proj")
	if err != nil || store == nil {
		t.Fatalf("memoryGate = %v, %v", store, err)
	}
	if budget.Lines != 7 || budget.Chars != 700 {
		t.Fatalf("budget = %+v", budget)
	}
}

func TestMemoryGateRefusals(t *testing.T) {
	data, root := t.TempDir(), t.TempDir()
	dp := newTestDeps(t)
	if _, _, err := memoryGate(dp, sessionconfig.Settings{}, data, root, "proj"); !errors.Is(err, memory.ErrMemoryOff) || !strings.Contains(err.Error(), "memory.repositories") {
		t.Fatalf("not listed: %v", err)
	}
	store, _, err := memoryGate(dp, memorySettings(root), data, root, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOff("operator", "paused"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memoryGate(dp, memorySettings(root), data, root, "proj"); !errors.Is(err, memory.ErrMemoryOff) || !strings.Contains(err.Error(), "memory on") {
		t.Fatalf("marker: %v", err)
	}
	if _, _, err := memoryGate(dp, memorySettings(root), data, root, "../x"); err == nil {
		t.Fatal("hostile project accepted")
	}
}

func TestKillSwitchStopsMemoryVerbs(t *testing.T) {
	data, root := t.TempDir(), t.TempDir()
	now := func() string { return "2026-10-09T00:00:00Z" }
	if err := release.Engage(data, "proj", "operator", "halt", now); err != nil {
		t.Fatal(err)
	}
	_, _, err := memoryGate(newTestDeps(t), memorySettings(root), data, root, "proj")
	if !errors.Is(err, memory.ErrMemoryOff) || !strings.Contains(err.Error(), "kill switch") {
		t.Fatalf("engaged with the switch on: %v", err)
	}
	if _, statErr := memory.Open(data, "other"); statErr != nil {
		t.Fatal(statErr)
	}
}
