package main

import (
	"os"
	"testing"

	"buildgate/internal/sandbox"
)

// testSandboxWorkerUID returns a valid worker UID that is distinct from the
// test process. Production keeps its fixed default; this selector only keeps
// tests that launch sandboxes independent of the UID of their host process.
func testSandboxWorkerUID() int {
	uid := os.Getuid()
	if sandbox.DefaultWorkerUID != uid {
		return sandbox.DefaultWorkerUID
	}
	if uid != 1 {
		return 1
	}
	return 2
}

func TestTestSandboxWorkerUIDIsValidAndHostDistinct(t *testing.T) {
	selected := testSandboxWorkerUID()
	if selected <= 0 || selected > 1<<31-1 {
		t.Fatalf("testSandboxWorkerUID() = %d, want 1..2^31-1", selected)
	}
	if selected == os.Getuid() {
		t.Fatalf("testSandboxWorkerUID() = host UID %d", selected)
	}
	if sandbox.DefaultWorkerUID != os.Getuid() && selected != sandbox.DefaultWorkerUID {
		t.Fatalf("testSandboxWorkerUID() = %d, want default %d when default is host-distinct", selected, sandbox.DefaultWorkerUID)
	}
}
