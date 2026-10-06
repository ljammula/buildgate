package harness

import (
	"fmt"
	"sort"
	"strings"
)

// Harness names. Each has one Descriptor in registry and one adapter in
// agent/pi/scripts/harness_adapters.py (TestNamesMatchThePythonAdapterRegistry
// pins the two lists together).
const (
	Pi      = "pi"
	Pifork  = "pifork"
	Codex   = "codex"
	Copilot = "copilot"
)

// Wire APIs a harness can speak to the relay; the values are the
// sessionconfig model `api` spellings.
const (
	wireOpenAICompletions = "openai-completions"
	wireOpenAIResponses   = "openai-responses"
	wireAnthropicMessages = "anthropic-messages"
)

// nodeBinary is where the worker images install Node, which runs pi, a Pi
// fork and the Codex CLI's launcher.
const nodeBinary = "/usr/local/bin/node"

// Descriptor is everything factoryd needs to know about one coding-agent CLI
// (a "harness") outside the Python adapter that drives it.
type Descriptor struct {
	Name string
	// Binary is the executable the sandbox image must contain.
	Binary string
	// WireAPIs are the model `api` values the harness can speak; a role whose
	// resolved model uses another api cannot use this harness.
	WireAPIs []string
	// WorkerEnv is the environment a sandboxed worker running this harness
	// needs beyond the common worker environment.
	WorkerEnv []string
	// ModelBinaries are the absolute paths, inside the sandbox image, of the
	// executables that call the model: the only ones the sandbox runtime's
	// network policy lets reach the role's route. Empty for a harness the
	// sandbox runtime does not run.
	ModelBinaries []string
	// RequiresSandboxImage marks a harness the canonical worker image does not
	// contain: its role needs an explicit sandbox_image.
	RequiresSandboxImage bool
	// RequiresWorkerModel marks a harness that cannot run without the role's
	// worker model id.
	RequiresWorkerModel bool
}

// SupportsAPI reports whether the harness can speak api to the relay.
func (d Descriptor) SupportsAPI(api string) bool {
	for _, w := range d.WireAPIs {
		if w == api {
			return true
		}
	}
	return false
}

// registry is the closed, compiled-in set of harnesses. It is deliberately not
// extensible at runtime: a new harness is added here, in code, reviewed like
// any other change -- never named by an operator- or developer-supplied path,
// script, or interpreter. See safety-contract.md's containment invariants.
var registry = map[string]Descriptor{
	Pi: {
		Name:          Pi,
		Binary:        "pi",
		WireAPIs:      []string{wireOpenAICompletions, wireOpenAIResponses, wireAnthropicMessages},
		ModelBinaries: []string{nodeBinary},
	},
	// A fork of Pi the operator packages into their own worker image (`make
	// pifork-image`): the image puts the fork's launcher at
	// /usr/local/bin/pifork, and that launcher sets whatever else the fork
	// needs. See doc/designs/pifork-harness.md for the image contract.
	Pifork: {
		Name:                 Pifork,
		Binary:               "pifork",
		WireAPIs:             []string{wireOpenAICompletions, wireOpenAIResponses, wireAnthropicMessages},
		WorkerEnv:            []string{"PI_CODING_AGENT_DIR=/home/worker/.pi/agent"},
		ModelBinaries:        []string{nodeBinary},
		RequiresSandboxImage: true,
		RequiresWorkerModel:  true,
	},
	// Codex CLI speaks only the Responses API. Its state directory, relay
	// provider and telemetry/plugin switches are set by CodexAdapter in the
	// worker (agent/pi/scripts/harness_adapters.py), so no WorkerEnv.
	Codex: {
		Name:     Codex,
		Binary:   "codex",
		WireAPIs: []string{wireOpenAIResponses},
		// codex.js runs under node and starts the native binary the npm
		// package ships below its own directory.
		ModelBinaries:       []string{nodeBinary, "/usr/local/lib/node_modules/@openai/codex/**"},
		RequiresWorkerModel: true,
	},
	// Copilot CLI in bring-your-own-key mode against the run's relay; its
	// provider, offline and state-directory environment is set by
	// CopilotAdapter in the worker.
	Copilot: {
		Name:                Copilot,
		Binary:              "copilot",
		WireAPIs:            []string{wireOpenAICompletions, wireOpenAIResponses, wireAnthropicMessages},
		RequiresWorkerModel: true,
	},
}

// Lookup returns the descriptor for name, lower-cased and trimmed; "" is Pi.
// An unknown name is an error naming the valid choices.
func Lookup(name string) (Descriptor, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = Pi
	}
	d, ok := registry[key]
	if !ok {
		return Descriptor{}, fmt.Errorf("unsupported harness %q (want one of: %s)", name, strings.Join(Names(), ", "))
	}
	// Copy the slices so a caller cannot mutate the registry.
	d.WireAPIs = append([]string(nil), d.WireAPIs...)
	d.ModelBinaries = append([]string(nil), d.ModelBinaries...)
	d.WorkerEnv = append([]string(nil), d.WorkerEnv...)
	return d, nil
}

// Names returns every registered harness name, sorted. It must equal the
// Python adapter registry's ADAPTERS keys
// (agent/pi/scripts/harness_adapters.py).
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ModelBinaries returns, sorted and without repeats, the executables of
// every harness that call a model. The worker image carries all of them and
// a role's route and credential are the same whichever one runs, so a
// sandbox's network policy admits the set rather than one harness's.
func ModelBinaries() []string {
	seen := map[string]bool{}
	var all []string
	for _, d := range registry {
		for _, path := range d.ModelBinaries {
			if !seen[path] {
				seen[path] = true
				all = append(all, path)
			}
		}
	}
	sort.Strings(all)
	return all
}
