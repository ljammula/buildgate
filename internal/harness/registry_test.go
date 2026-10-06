package harness

import (
	"strings"
	"testing"
)

func TestLookupDefaultsToPi(t *testing.T) {
	for _, name := range []string{"", "  ", "PI", " pi "} {
		d, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if d.Name != Pi || d.Binary != "pi" || d.RequiresSandboxImage || d.RequiresWorkerModel || len(d.WorkerEnv) != 0 {
			t.Fatalf("Lookup(%q) = %#v, want the plain Pi descriptor", name, d)
		}
	}
}

func TestLookupPiforkDescriptor(t *testing.T) {
	d, err := Lookup("PIFORK")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !d.RequiresSandboxImage || !d.RequiresWorkerModel || d.Binary != "pifork" {
		t.Fatalf("pifork descriptor = %#v, want sandbox and model requirements", d)
	}
	for _, api := range []string{"openai-completions", "openai-responses", "anthropic-messages"} {
		if !d.SupportsAPI(api) {
			t.Errorf("pifork does not support %s", api)
		}
	}
	if d.SupportsAPI("gemini") {
		t.Error("pifork unexpectedly supports gemini")
	}
}

func TestLookupRejectsUnknownNamingTheChoices(t *testing.T) {
	_, err := Lookup("unknown")
	if err == nil || !strings.Contains(err.Error(), "codex, copilot, pi, pifork") {
		t.Fatalf("Lookup(unknown) error = %v, want one naming the valid choices", err)
	}
}

func TestLookupCodexDescriptor(t *testing.T) {
	d, err := Lookup(" Codex ")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if d.Name != Codex || d.Binary != "codex" || !d.RequiresWorkerModel || d.RequiresSandboxImage || len(d.WorkerEnv) != 0 {
		t.Fatalf("Codex descriptor = %#v", d)
	}
	// Codex speaks only the Responses API.
	if !d.SupportsAPI("openai-responses") {
		t.Error("Codex does not support openai-responses")
	}
	for _, api := range []string{"openai-completions", "anthropic-messages"} {
		if d.SupportsAPI(api) {
			t.Errorf("Codex unexpectedly supports %s", api)
		}
	}
}

func TestLookupCopilotDescriptor(t *testing.T) {
	d, err := Lookup("copilot")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if d.Name != Copilot || d.Binary != "copilot" || !d.RequiresWorkerModel || d.RequiresSandboxImage || len(d.WorkerEnv) != 0 {
		t.Fatalf("Copilot descriptor = %#v", d)
	}
	for _, api := range []string{"openai-completions", "openai-responses", "anthropic-messages"} {
		if !d.SupportsAPI(api) {
			t.Errorf("Copilot does not support %s", api)
		}
	}
}

func TestWorkerEnvIsIsolatedForPifork(t *testing.T) {
	d, _ := Lookup("pifork")
	got := strings.Join(d.WorkerEnv, "\n")
	for _, want := range []string{
		"PI_CODING_AGENT_DIR=/home/worker/.pi/agent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pifork worker environment = %q, missing %q", got, want)
		}
	}
}

func TestLookupReturnsACopy(t *testing.T) {
	d, _ := Lookup("pifork")
	d.WorkerEnv[0] = "MUTATED=1"
	d.WireAPIs[0] = "mutated"
	again, _ := Lookup("pifork")
	if again.WorkerEnv[0] == "MUTATED=1" || again.WireAPIs[0] == "mutated" {
		t.Fatal("Lookup exposes the registry's own slices")
	}
}

func TestModelBinariesAreAbsolutePathsAndCopilotHasNone(t *testing.T) {
	want := map[string][]string{
		Pi:      {"/usr/local/bin/node"},
		Pifork:  {"/usr/local/bin/node"},
		Codex:   {"/usr/local/bin/node", "/usr/local/lib/node_modules/@openai/codex/**"},
		Copilot: nil,
	}
	for _, name := range Names() {
		d, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		expected, known := want[name]
		if !known {
			t.Errorf("harness %q has no expected model binaries in this test", name)
			continue
		}
		if len(d.ModelBinaries) != len(expected) {
			t.Errorf("%s ModelBinaries = %v, want %v", name, d.ModelBinaries, expected)
			continue
		}
		for i := range expected {
			if d.ModelBinaries[i] != expected[i] {
				t.Errorf("%s ModelBinaries = %v, want %v", name, d.ModelBinaries, expected)
			}
		}
	}
}

func TestModelBinariesIsTheSortedUnion(t *testing.T) {
	want := []string{"/usr/local/bin/node", "/usr/local/lib/node_modules/@openai/codex/**"}
	got := ModelBinaries()
	if len(got) != len(want) {
		t.Fatalf("ModelBinaries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ModelBinaries() = %v, want %v", got, want)
		}
	}
}
