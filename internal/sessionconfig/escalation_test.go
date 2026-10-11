package sessionconfig

import (
	"strings"
	"testing"

	"buildgate/internal/harness"
)

// escalationSettings is routesModeSettings with a second allowed model,
// "sol", that supports every level, as roles.execution.escalation at high.
func escalationSettings() Settings {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, Reasoning: true, ThinkingLevelMap: map[string]string{"max": "max"}}
	s.Models["sol"] = Model{ID: "gpt-5.6-sol", Routes: []string{"litellm"}, Reasoning: true, ThinkingLevelMap: map[string]string{"max": "max"}}
	s.Roles.Execution.Thinking = "max"
	s.Roles.Execution.Allowed = []string{"luna", "sol"}
	s.Roles.Execution.Escalation = &Escalation{Model: "sol", Thinking: "high"}
	return s
}

// TestValidateRoutingAcceptsAnEscalation: an allowed model, with or without
// its own level, and the role's own model at another level.
func TestValidateRoutingAcceptsAnEscalation(t *testing.T) {
	s := escalationSettings()
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("escalation to sol at high: %v", err)
	}
	s.Roles.Execution.Escalation = &Escalation{Model: "sol"}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("escalation with no level (the role's max, which sol declares): %v", err)
	}
	s.Roles.Execution.Thinking = "low"
	s.Roles.Execution.Escalation = &Escalation{Model: "luna", Thinking: "max"}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("escalation to the role's own model at a higher level: %v", err)
	}
}

// TestValidateRoutingRefusesABadEscalation: the escalation's model must be in
// roles.execution.allowed, where every check of an allowed model covers it,
// and its level is checked against that model.
func TestValidateRoutingRefusesABadEscalation(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(s *Settings)
		want   []string
	}{
		"unknown model": {
			func(s *Settings) { s.Roles.Execution.Escalation.Model = "nope" },
			[]string{"roles.execution.escalation", `"nope"`, "add it to allowed"},
		},
		"a models: entry that is not in allowed": {
			func(s *Settings) { s.Roles.Execution.Allowed = []string{"luna"} },
			[]string{"roles.execution.escalation", `"sol"`, "roles.execution.allowed", "add it to allowed"},
		},
		"not in allowed when allowed is unset": {
			func(s *Settings) { s.Roles.Execution.Allowed = nil },
			[]string{"roles.execution.escalation", `"sol"`, "add it to allowed"},
		},
		"no model": {
			func(s *Settings) { s.Roles.Execution.Escalation.Model = "" },
			[]string{"roles.execution.escalation", "model is required"},
		},
		"model on a route that is not configured": {
			func(s *Settings) {
				s.Models["sol"] = Model{ID: "gpt-5.6-sol", Routes: []string{"gone"}, Reasoning: true}
			},
			[]string{"models.sol", `"gone"`},
		},
		"level that is no level": {
			func(s *Settings) { s.Roles.Execution.Escalation.Thinking = "extreme" },
			[]string{"roles.execution.escalation", `"extreme"`, "not a supported level"},
		},
		"level on a model without reasoning": {
			func(s *Settings) {
				s.Roles.Execution.Thinking = ""
				s.Models["sol"] = Model{ID: "gpt-5.6-sol", Routes: []string{"litellm"}}
			},
			[]string{"roles.execution.escalation", "models.sol.reasoning"},
		},
		"max the model would clamp": {
			func(s *Settings) {
				s.Roles.Execution.Thinking = ""
				s.Models["sol"] = Model{ID: "gpt-5.6-sol", Routes: []string{"litellm"}, Reasoning: true}
				s.Roles.Execution.Escalation.Thinking = "max"
			},
			[]string{"roles.execution.escalation", "models.sol.thinking_level_map.max"},
		},
		"the role's level, inherited, that the model would clamp": {
			func(s *Settings) {
				s.Models["sol"] = Model{ID: "gpt-5.6-sol", Routes: []string{"litellm"}, Reasoning: true}
				s.Roles.Execution.Escalation.Thinking = ""
			},
			[]string{"roles.execution", "models.sol.thinking_level_map.max"},
		},
		"same backend as the review model": {
			func(s *Settings) {
				s.Models["reviewer"] = Model{ID: "gpt-5.6-sol", Routes: []string{"litellm"}}
				s.Roles.Review = &RoleConfig{Model: "reviewer"}
			},
			[]string{"roles.review", `"reviewer"`, "roles.execution.allowed", `"sol"`, "allow_shared_model"},
		},
		"on the planning role": {
			func(s *Settings) {
				s.Roles.Planning = &RoleConfig{Model: "luna", Escalation: &Escalation{Model: "sol"}}
			},
			[]string{"roles.planning.escalation", "only roles.execution"},
		},
		"on the review role": {
			func(s *Settings) {
				s.Models["reviewer"] = Model{ID: "reviewer-id", Routes: []string{"litellm"}}
				s.Roles.Review = &RoleConfig{Model: "reviewer", Escalation: &Escalation{Model: "sol"}}
			},
			[]string{"roles.review.escalation", "only roles.execution"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := escalationSettings()
			tc.mutate(&s)
			err := ValidateRouting(s)
			if err == nil {
				t.Fatal("ValidateRouting = nil, want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, missing %q", err, want)
				}
			}
		})
	}
}

// TestValidateRoutingEscalationSharingTheReviewBackendIsWaivable: the
// review-independence rule covers the escalation's model, under the same
// waiver.
func TestValidateRoutingEscalationSharingTheReviewBackendIsWaivable(t *testing.T) {
	s := escalationSettings()
	s.Models["reviewer"] = Model{ID: "gpt-5.6-sol", Routes: []string{"litellm"}}
	s.Roles.Review = &RoleConfig{Model: "reviewer", AllowSharedModel: true}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting = %v, want nil under allow_shared_model", err)
	}
}

// TestValidateRoutingRefusesAnEscalationModelTheHarnessCannotRun injects a
// harness that speaks only anthropic-messages: the role's own model is on
// that api and passes, the escalation's is not.
func TestValidateRoutingRefusesAnEscalationModelTheHarnessCannotRun(t *testing.T) {
	prev := harnessLookup
	t.Cleanup(func() { harnessLookup = prev })
	harnessLookup = func(name string) (harness.Descriptor, error) {
		if name == "narrow" {
			return harness.Descriptor{Name: "narrow", WireAPIs: []string{"anthropic-messages"}}, nil
		}
		return prev(name)
	}
	s := escalationSettings()
	luna := s.Models["luna"]
	luna.API = "anthropic-messages"
	s.Models["luna"] = luna
	s.Roles.Execution.Harness = "narrow"
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting accepted an escalation model the role's harness cannot speak to")
	}
	for _, want := range []string{"roles.execution", `"narrow"`, `"sol"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, missing %q", err, want)
		}
	}
}

// TestValidateRunExecution: a run names a model within allowed, as a request
// does, and a level only when the config gives it to that model.
func TestValidateRunExecution(t *testing.T) {
	s := escalationSettings()
	if err := ValidateRequestModels(s, map[string]string{"execution": "sol"}); err != nil {
		t.Errorf("ValidateRequestModels(sol) = %v, want nil: the escalation's model is in roles.execution.allowed", err)
	}
	for name, tc := range map[string]struct {
		model, thinking string
		refused         string
	}{
		"nothing chosen":                             {"", "", ""},
		"the escalation's model at its level":        {"sol", "high", ""},
		"the escalation's model at the role's level": {"sol", "max", ""},
		"the escalation's model, level left":         {"sol", "", ""},
		"the role's level, said aloud":               {"", "max", ""},
		"the escalation's level on the role's model": {"", "high", "-execution-thinking"},
		"a level the config does not name":           {"sol", "low", "-execution-thinking"},
		"a model outside allowed":                    {"other", "", "-execution-model"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateRunExecution(s, tc.model, tc.thinking)
			if tc.refused == "" && err != nil {
				t.Fatalf("ValidateRunExecution(%q, %q) = %v, want nil", tc.model, tc.thinking, err)
			}
			if tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Fatalf("ValidateRunExecution(%q, %q) = %v, want a refusal naming %s", tc.model, tc.thinking, err, tc.refused)
			}
		})
	}
	if err := ValidateRunExecution(Settings{}, "", "high"); err == nil {
		t.Error("ValidateRunExecution accepted a level with no roles.execution configured")
	}
	if err := ValidateRunExecution(Settings{}, "sol", ""); err == nil || !strings.Contains(err.Error(), "-execution-model") {
		t.Errorf("ValidateRunExecution with no routing config = %v, want -execution-model refused", err)
	}
}

// TestLoadParsesAnEscalation: the key round-trips through the YAML schema.
func TestLoadParsesAnEscalation(t *testing.T) {
	cfg, err := Load(writeConfig(t, `routes:
  local:
    upstream: http://127.0.0.1:8080
    allow_no_credential: true
models:
  luna:
    id: gpt-5.6-luna
    routes: [local]
    reasoning: true
  sol:
    id: gpt-5.6-sol
    routes: [local]
    reasoning: true
roles:
  execution:
    model: luna
    thinking: medium
    allowed: [luna, sol]
    escalation:
      model: sol
      thinking: high
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Roles.Execution
	if got.EscalationModel() != "sol" || got.EscalationThinking() != "high" {
		t.Errorf("escalation = %q at %q, want sol at high", got.EscalationModel(), got.EscalationThinking())
	}
	if err := ValidateRouting(Settings{Routes: cfg.Routes, Models: cfg.Models, Roles: cfg.Roles}); err != nil {
		t.Errorf("ValidateRouting of the loaded config: %v", err)
	}
}
