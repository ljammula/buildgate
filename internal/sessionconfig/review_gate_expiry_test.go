package sessionconfig

import (
	"strings"
	"testing"
	"time"
)

func TestReviewGateExpiryIsOffByDefaultAndParsesADuration(t *testing.T) {
	if got := DefaultSettings().ReviewGateExpiry; got != 0 {
		t.Errorf("default ReviewGateExpiry = %v, want 0 (a gate never expires)", got)
	}
	cfg, err := Load(writeConfig(t, "review_gate_expiry: 36h\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if s.ReviewGateExpiry != 36*time.Hour {
		t.Errorf("ReviewGateExpiry = %v, want 36h", s.ReviewGateExpiry)
	}
	unset, err := Load(writeConfig(t, "hitl_reminder_interval: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := unset.ApplySettings(DefaultSettings()); err != nil || s.ReviewGateExpiry != 0 {
		t.Errorf("a config without the key: ReviewGateExpiry = %v err %v, want 0", s.ReviewGateExpiry, err)
	}
}

func TestReviewGateExpiryRefusesWhatIsNotADuration(t *testing.T) {
	for _, body := range []string{"review_gate_expiry: soon\n", "review_gate_expiry: -1h\n"} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "review_gate_expiry") {
			t.Errorf("%q: ApplySettings = %v, want a refusal naming review_gate_expiry", body, err)
		}
	}
}

func TestPRBudgetLinesDefaultsAndValidates(t *testing.T) {
	if got := DefaultSettings().PRBudgetLines; got != 400 {
		t.Errorf("default PRBudgetLines = %d, want 400", got)
	}
	for body, want := range map[string]int{"pr_budget_lines: 250\n": 250, "pr_budget_lines: 0\n": 0, "hitl_reminder_interval: 10m\n": 400} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil || s.PRBudgetLines != want {
			t.Errorf("%q: PRBudgetLines = %d err %v, want %d", body, s.PRBudgetLines, err, want)
		}
	}
	cfg, err := Load(writeConfig(t, "pr_budget_lines: -1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "pr_budget_lines") {
		t.Errorf("negative: ApplySettings = %v, want a refusal naming pr_budget_lines", err)
	}
}

func TestReleaseChangedLinesKeyReplacesInsertionsKey(t *testing.T) {
	cfg, err := Load(writeConfig(t, "release_max_changed_lines: 123\n"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil || settings.ReleaseMaxChangedLines != 123 {
		t.Fatalf("changed-line cap = %d, %v, want 123", settings.ReleaseMaxChangedLines, err)
	}
	if _, err := Load(writeConfig(t, "release_max_insertions: 123\n")); err == nil {
		t.Fatal("retired release_max_insertions key was accepted")
	}
}

func TestPlanReviewIsOffByDefaultAndTurnsOn(t *testing.T) {
	if DefaultSettings().PlanReview || (Settings{}).PlanReview {
		t.Error("plan_review on by default, want off")
	}
	for body, want := range map[string]bool{"plan_review: true\n": true, "plan_review: false\n": false, "pr_budget_lines: 250\n": false} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil || s.PlanReview != want {
			t.Errorf("%q: PlanReview = %t err %v, want %t", body, s.PlanReview, err, want)
		}
	}
}

func TestReviewAdviceRoundsDefaultsToThreeAndIsBounded(t *testing.T) {
	if got := DefaultSettings().ReviewAdviceRounds; got != 3 {
		t.Errorf("default advice rounds = %d, want 3", got)
	}
	for body, want := range map[string]int{"review_advice_rounds: 0\n": 0, "review_advice_rounds: 1\n": 1, "review_advice_rounds: 3\n": 3, "pr_budget_lines: 250\n": 3} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil || s.ReviewAdviceRounds != want {
			t.Errorf("%q: ReviewAdviceRounds = %d err %v, want %d", body, s.ReviewAdviceRounds, err, want)
		}
	}
	for _, body := range []string{"review_advice_rounds: 4\n", "review_advice_rounds: -1\n"} {
		cfg, err := Load(writeConfig(t, body))
		if err == nil {
			_, err = cfg.ApplySettings(DefaultSettings())
		}
		if err == nil || !strings.Contains(err.Error(), "review_advice_rounds") {
			t.Errorf("%q: %v, want a refusal naming review_advice_rounds", body, err)
		}
	}
	if _, err := Load(writeConfig(t, "review_advice_round: true\n")); err == nil {
		t.Error("the retired review_advice_round key loaded, want a refusal")
	}
}

func TestRoundSessionContinuityDefaultsOnAndTurnsOff(t *testing.T) {
	if !DefaultSettings().RoundSessionContinuity {
		t.Error("session continuity off by default, want on")
	}
	for body, want := range map[string]bool{"round_session_continuity: false\n": false, "round_session_continuity: true\n": true, "pr_budget_lines: 250\n": true} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil || s.RoundSessionContinuity != want {
			t.Errorf("%q: RoundSessionContinuity = %t err %v, want %t", body, s.RoundSessionContinuity, err, want)
		}
	}
	if _, err := Load(writeConfig(t, "round_session_continuity: sometimes\n")); err == nil {
		t.Error("round_session_continuity: sometimes loaded, want a refusal")
	}
}

// A review writes no test the factory runs (SC-022): the retired
// review_reproductions key is refused, not ignored.
func TestRetiredReviewReproductionsKeyIsRefused(t *testing.T) {
	if _, err := Load(writeConfig(t, "review_reproductions: true\n")); err == nil {
		t.Fatal("retired review_reproductions key was accepted")
	}
}

// roles.challenger (SC-022) is an ordinary role: it loads, names a model the
// routes configure, and is absent by default.
func TestChallengerRoleIsValidatedLikeTheOthers(t *testing.T) {
	if DefaultSettings().Roles != nil && DefaultSettings().Roles.Challenger != nil {
		t.Error("a challenger role is set by default")
	}
	cfg, err := Load(writeConfig(t, "roles:\n  challenger:\n    model: terra\n    thinking: high\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := cfg.ApplySettings(DefaultSettings())
	if err != nil || s.Roles == nil || s.Roles.Challenger == nil || s.Roles.Challenger.Model != "terra" {
		t.Fatalf("settings = %+v, %v; want roles.challenger read", s.Roles, err)
	}
	if _, err := Load(writeConfig(t, "roles:\n  challenger:\n    nonsense: 1\n")); err == nil {
		t.Error("an unknown roles.challenger key was accepted")
	}
}
