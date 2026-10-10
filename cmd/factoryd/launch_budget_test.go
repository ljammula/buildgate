package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/requestdriver/requestdrivertest"
)

// --- Driver wiring: every launch point calls checkLaunchBudget before
// starting a container, and quarantines (never launching) once it fires.

// TestFactoryDCostPrintsBudgetSection covers the `factoryd cost` addition:
// once a budget key is configured (via -config), the report includes a
// budgets section naming the configured limit and, for a monthly budget,
// the month-to-date spend.
func TestFactoryDCostPrintsBudgetSection(t *testing.T) {
	dataDir := t.TempDir()
	// Mid-month, not time.Now(): in the first hour of a UTC month the
	// fixture's now-1h falls in the previous month and drops out of the
	// month-to-date sum.
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	requestdrivertest.SaveRunFixture(t, dataDir, "run-direct", "", now.Add(-time.Hour).Format(time.RFC3339), 500, 750_000)

	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("monthly_cost_budget_micro_usd: 5000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := buildCostReport(dataDir, "", "")
	if err != nil {
		t.Fatalf("buildCostReport: %v", err)
	}
	settings, err := loadSettingsForConfig(configPath)
	if err != nil {
		t.Fatalf("loadSettingsForConfig: %v", err)
	}
	budgets, err := buildBudgetReport(dataDir, settings, now)
	if err != nil {
		t.Fatalf("buildBudgetReport: %v", err)
	}
	if budgets == nil {
		t.Fatal("buildBudgetReport = nil, want a populated report (monthly_cost_budget_micro_usd is configured)")
	}
	if budgets.MonthlyCostBudgetMicroUSD != 5_000_000 {
		t.Errorf("MonthlyCostBudgetMicroUSD = %d, want 5000000", budgets.MonthlyCostBudgetMicroUSD)
	}
	if budgets.MonthToDateCostMicroUSD != 750_000 {
		t.Errorf("MonthToDateCostMicroUSD = %d, want 750000", budgets.MonthToDateCostMicroUSD)
	}
	_ = rep
}
