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

func TestReviewReproductionsDefaultsOffAndTurnsOn(t *testing.T) {
	if DefaultSettings().ReviewReproductions || (Settings{}).ReviewReproductions {
		t.Error("reproductions on by default, want off")
	}
	for body, want := range map[string]bool{"review_reproductions: false\n": false, "review_reproductions: true\n": true, "pr_budget_lines: 250\n": false} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load %q: %v", body, err)
		}
		s, err := cfg.ApplySettings(DefaultSettings())
		if err != nil || s.ReviewReproductions != want {
			t.Errorf("%q: ReviewReproductions = %t err %v, want %t", body, s.ReviewReproductions, err, want)
		}
	}
	if _, err := Load(writeConfig(t, "review_reproductions: sometimes\n")); err == nil {
		t.Error("review_reproductions: sometimes loaded, want a refusal")
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

func TestReviewAdviceRoundsDefaultsOffAndIsBounded(t *testing.T) {
	if DefaultSettings().ReviewAdviceRounds != 0 || (Settings{}).ReviewAdviceRounds != 0 {
		t.Error("advice rounds on by default, want 0")
	}
	for body, want := range map[string]int{"review_advice_rounds: 0\n": 0, "review_advice_rounds: 1\n": 1, "review_advice_rounds: 3\n": 3, "review_reproductions: true\n": 0} {
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
