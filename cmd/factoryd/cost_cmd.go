package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// newCostFlags builds `factoryd cost`'s FlagSet in isolation from parsing,
// so USAGE.md's doc-vs-flag drift test (TestUSAGEDocFlagsExistOnSubcommand)
// can enumerate its real flags without executing the command -- the same
// shape newStatusFlags/newWatchFlags use.
func newCostFlags() (flags *flag.FlagSet, dataDir, requestID, since, configPath *string, jsonOutput *bool) {
	flags = flag.NewFlagSet("cost", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable run records")
	requestID = flags.String("request", "", "only show this one request's cost rollup")
	since = flags.String("since", "", "only include requests submitted on/after this date (YYYY-MM-DD); empty includes every request")
	jsonOutput = flags.Bool("json", false, "print machine-readable JSON instead of a table")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

// costReport is `factoryd cost`'s own rollup: one or more requests' own
// api.CostSummary (computed by api.Server.ComputeCostSummary, never
// recomputed here -- see this file's own doc comment on why) merged into a
// single per-(role, model) breakdown plus the request-level human-cost
// proxy counts USAGE.md's cost section documents.
type costReport struct {
	ByModel                       []api.ModelUsage `json:"by_model,omitempty"`
	Spec                          float64          `json:"spec"`
	Plan                          float64          `json:"plan"`
	Oracle                        float64          `json:"oracle,omitempty"`
	Runs                          float64          `json:"runs"`
	Total                         float64          `json:"total"`
	Complete                      bool             `json:"complete"`
	TokensComplete                bool             `json:"tokens_complete"`
	AcceptedTickets               int              `json:"accepted_tickets"`
	CostPerAcceptedTicketMicroUSD int64            `json:"cost_per_accepted_ticket_micro_usd"`
	// QuarantinedTickets/RejectedSpecs/RejectedPlans are the human-cost
	// proxy this command reports alongside dollars: how much of this
	// spend needed a human to notice something went wrong. Each is
	// exactly derivable from durable records (a ticket's own run.State,
	// and Request.Rejections' own Stage()) -- unlike, say, "operator
	// review minutes", which nothing in this repo's records captures at
	// all, so it is not attempted here.
	QuarantinedTickets int `json:"quarantined_tickets"`
	RejectedSpecs      int `json:"rejected_specs"`
	RejectedPlans      int `json:"rejected_plans"`
	// RequestCount is how many requests contributed to this report --
	// always 1 for -request, otherwise how many of request.List's own
	// requests passed the -since filter.
	RequestCount int `json:"request_count"`
	// Budgets is nil unless at least one request_*/monthly_* budget key
	// is configured (see buildBudgetReport) -- omitted from JSON output
	// entirely for an operator who never opted into budgets.
	Budgets *budgetReport `json:"budgets,omitempty"`
}

// costModelKey groups costReport.ByModel across requests -- (Role, Model),
// mirroring internal/api's own unexported modelUsageKey, which this
// package cannot reach directly.
type costModelKey struct {
	Role  string
	Model string
}

// addRequestCostSummary folds one request's own api.CostSummary (computed
// by the same api.Server.ComputeCostSummary the API's GET /requests uses --
// this command never recomputes the rollup arithmetic itself) into rep,
// plus this request's own quarantined-ticket/rejected-spec/rejected-plan
// counts (derived from its own run records and Rejections, not from
// CostSummary, which carries neither).
func (rep *costReport) addRequestCostSummary(dataDir string, req *request.Request, cs api.CostSummary, byModel map[costModelKey]*api.ModelUsage, order *[]costModelKey) {
	rep.Spec += cs.Spec
	rep.Plan += cs.Plan
	rep.Oracle += cs.Oracle
	rep.Runs += cs.Runs
	rep.Total += cs.Total
	rep.AcceptedTickets += cs.AcceptedTickets
	if !cs.Complete {
		rep.Complete = false
	}
	if !cs.TokensComplete {
		rep.TokensComplete = false
	}
	for _, mu := range cs.ByModel {
		key := costModelKey{Role: mu.Role, Model: mu.Model}
		existing, ok := byModel[key]
		if !ok {
			copied := mu
			byModel[key] = &copied
			*order = append(*order, key)
			continue
		}
		existing.Tokens += mu.Tokens
		existing.CostMicroUSD += mu.CostMicroUSD
	}
	for _, ticket := range req.Tickets {
		if ticket.RunID == "" {
			continue
		}
		if loadedRun, err := run.Load(dataDir, ticket.RunID); err == nil && loadedRun.State == run.StateQuarantined {
			rep.QuarantinedTickets++
		}
	}
	for _, rej := range req.Rejections {
		switch rej.Stage() {
		case request.StateSpecReview:
			rep.RejectedSpecs++
		case request.StatePlanReview:
			rep.RejectedPlans++
		}
	}
	rep.RequestCount++
}

// finish sorts byModel into rep.ByModel (role, then model -- the same
// deterministic order api's own modelUsageAccumulator.finish uses) and
// computes CostPerAcceptedTicketMicroUSD from rep's own already-summed
// Total/AcceptedTickets: the same "total request spend incl. drafting,
// failed and corrective rounds, / accepted tickets, 0 when none accepted"
// contract api.CostSummary.CostPerAcceptedTicketMicroUSD documents,
// applied across every request this report covers rather than one.
func (rep *costReport) finish(byModel map[costModelKey]*api.ModelUsage, order []costModelKey) {
	rep.ByModel = make([]api.ModelUsage, 0, len(order))
	for _, key := range order {
		rep.ByModel = append(rep.ByModel, *byModel[key])
	}
	sort.Slice(rep.ByModel, func(i, j int) bool {
		if rep.ByModel[i].Role != rep.ByModel[j].Role {
			return rep.ByModel[i].Role < rep.ByModel[j].Role
		}
		return rep.ByModel[i].Model < rep.ByModel[j].Model
	})
	if rep.AcceptedTickets > 0 {
		rep.CostPerAcceptedTicketMicroUSD = int64(rep.Total * 1e6 / float64(rep.AcceptedTickets))
	}
}

func costMain(args []string) error {
	flags, dataDir, requestID, since, configPath, jsonOutput := newCostFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	// *configPath, not "": mirrors statusMain's own "-config wins"
	// -data-dir resolution.
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}

	rep, err := buildCostReport(*dataDir, *requestID, *since)
	if err != nil {
		return err
	}

	// budgetInfo is best-effort: a session config that fails to resolve
	// here must never fail `factoryd cost` itself, which has already
	// loaded (and would otherwise discard) a perfectly good cost report --
	// see loadSettingsForConfig's own doc comment on why it never fails
	// loudly for exactly this class of caller.
	settings, settingsErr := loadSettingsForConfig(*configPath)
	var budgets *budgetReport
	if settingsErr == nil {
		if b, err := buildBudgetReport(*dataDir, settings, time.Now()); err == nil {
			budgets = b
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if budgets != nil {
			rep.Budgets = budgets
		}
		return enc.Encode(rep)
	}
	printCostReport(os.Stdout, rep)
	if budgets != nil {
		printBudgetReport(os.Stdout, budgets)
	}
	return nil
}

// budgetReport is `factoryd cost`'s own optional section: the configured
// request/monthly budgets (only the ones actually set -- 0 means
// unlimited and is omitted, not printed as a fake "0") alongside the
// current month-to-date spend checkLaunchBudget itself would see. Printed
// only when at least one budget key is configured, so an operator who
// never opted into budgets sees no new output at all.
type budgetReport struct {
	RequestTokenBudget        int64 `json:"request_token_budget,omitempty"`
	RequestCostBudgetMicroUSD int64 `json:"request_cost_budget_micro_usd,omitempty"`
	MonthlyTokenBudget        int64 `json:"monthly_token_budget,omitempty"`
	MonthlyCostBudgetMicroUSD int64 `json:"monthly_cost_budget_micro_usd,omitempty"`
	MonthToDateTokens         int64 `json:"month_to_date_tokens"`
	MonthToDateCostMicroUSD   int64 `json:"month_to_date_cost_micro_usd"`
}

// buildBudgetReport returns nil, nil when no budget key is configured at
// all -- costMain then prints nothing further, matching this command's
// existing behavior for every operator who hasn't opted into budgets.
func buildBudgetReport(dataDir string, settings sessionconfig.Settings, now time.Time) (*budgetReport, error) {
	if settings.RequestTokenBudget <= 0 && settings.RequestCostBudgetMicroUSD <= 0 &&
		settings.MonthlyTokenBudget <= 0 && settings.MonthlyCostBudgetMicroUSD <= 0 {
		return nil, nil
	}
	rep := &budgetReport{
		RequestTokenBudget:        int64(settings.RequestTokenBudget),
		RequestCostBudgetMicroUSD: settings.RequestCostBudgetMicroUSD,
		MonthlyTokenBudget:        int64(settings.MonthlyTokenBudget),
		MonthlyCostBudgetMicroUSD: settings.MonthlyCostBudgetMicroUSD,
	}
	if settings.MonthlyTokenBudget > 0 || settings.MonthlyCostBudgetMicroUSD > 0 {
		tokens, costMicroUSD, err := requestdriver.MonthToDateSpend(dataDir, now)
		if err != nil {
			return nil, err
		}
		rep.MonthToDateTokens = tokens
		rep.MonthToDateCostMicroUSD = costMicroUSD
	}
	return rep, nil
}

// printBudgetReport renders rep as a plain-text section appended after
// printCostReport's own table -- see budgetReport's own doc comment for
// when this is called at all.
func printBudgetReport(w io.Writer, rep *budgetReport) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "budgets:")
	if rep.RequestTokenBudget > 0 {
		fmt.Fprintf(w, "  request_token_budget:           %d tokens\n", rep.RequestTokenBudget)
	}
	if rep.RequestCostBudgetMicroUSD > 0 {
		fmt.Fprintf(w, "  request_cost_budget_micro_usd:  $%.2f\n", float64(rep.RequestCostBudgetMicroUSD)/1e6)
	}
	if rep.MonthlyTokenBudget > 0 {
		fmt.Fprintf(w, "  monthly_token_budget:           %d tokens (month to date: %d)\n", rep.MonthlyTokenBudget, rep.MonthToDateTokens)
	}
	if rep.MonthlyCostBudgetMicroUSD > 0 {
		fmt.Fprintf(w, "  monthly_cost_budget_micro_usd:  $%.2f (month to date: $%.2f)\n", float64(rep.MonthlyCostBudgetMicroUSD)/1e6, float64(rep.MonthToDateCostMicroUSD)/1e6)
	}
}

// buildCostReport is costMain's own logic, split out from flag
// parsing/I/O so a test can exercise it directly against a fixture data
// dir without going through os.Args/os.Stdout. since is the raw -since
// flag value ("" for no filter, else YYYY-MM-DD).
func buildCostReport(dataDir, requestID, since string) (*costReport, error) {
	var sinceTime time.Time
	if since != "" {
		t, err := time.Parse("2006-01-02", since)
		if err != nil {
			return nil, fmt.Errorf("-since %q: %w (want YYYY-MM-DD)", since, err)
		}
		sinceTime = t
	}

	var requests []*request.Request
	if requestID != "" {
		r, err := request.Load(dataDir, requestID)
		if err != nil {
			return nil, fmt.Errorf("load request %q from %q: %w", requestID, dataDir, err)
		}
		requests = []*request.Request{r}
	} else {
		all, err := request.List(dataDir)
		if err != nil {
			return nil, fmt.Errorf("load requests from %q: %w", dataDir, err)
		}
		for _, r := range all {
			if !sinceTime.IsZero() {
				submitted, err := time.Parse(time.RFC3339Nano, r.SubmittedAt)
				if err != nil || submitted.Before(sinceTime) {
					continue
				}
			}
			requests = append(requests, r)
		}
	}

	s := api.NewServer(dataDir)
	// One dataDir/runs scan for every request this report covers, not one
	// per request -- see api.Server.ComputeCostSummary's own doc comment.
	runsByRequest, err := run.ListByRequestID(dataDir)
	if err != nil {
		return nil, fmt.Errorf("list runs from %q: %w", dataDir, err)
	}
	rep := &costReport{Complete: true, TokensComplete: true}
	byModel := map[costModelKey]*api.ModelUsage{}
	var order []costModelKey
	for _, r := range requests {
		cs := s.ComputeCostSummaryWithRuns(r, runsByRequest)
		rep.addRequestCostSummary(dataDir, r, cs, byModel, &order)
	}
	rep.finish(byModel, order)
	return rep, nil
}

// printCostReport renders rep as a plain-text table: role x model ->
// tokens, cost ($, 2 decimals), then totals and the human-cost proxy
// counts.
func printCostReport(w io.Writer, rep *costReport) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tMODEL\tTOKENS\tCOST")
	for _, mu := range rep.ByModel {
		role := mu.Role
		if role == "" {
			role = "unknown"
		}
		model := mu.Model
		if model == "" {
			model = "unknown"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t$%.2f\n", role, model, mu.Tokens, float64(mu.CostMicroUSD)/1e6)
	}
	tw.Flush()
	fmt.Fprintln(w)
	completeness := ""
	if !rep.Complete {
		completeness = " (lower bound -- some evidence was unreadable or missing)"
	}
	fmt.Fprintf(w, "requests:            %d\n", rep.RequestCount)
	fmt.Fprintf(w, "spec drafting:       $%.2f\n", rep.Spec)
	fmt.Fprintf(w, "plan drafting:       $%.2f\n", rep.Plan)
	if rep.Oracle != 0 {
		fmt.Fprintf(w, "oracle drafting:     $%.2f\n", rep.Oracle)
	}
	fmt.Fprintf(w, "ticket runs:         $%.2f\n", rep.Runs)
	fmt.Fprintf(w, "total:               $%.2f%s\n", rep.Total, completeness)
	fmt.Fprintf(w, "accepted tickets:    %d\n", rep.AcceptedTickets)
	if rep.AcceptedTickets > 0 {
		fmt.Fprintf(w, "cost/accepted ticket: $%.2f\n", float64(rep.CostPerAcceptedTicketMicroUSD)/1e6)
	} else {
		fmt.Fprintln(w, "cost/accepted ticket: n/a (no accepted tickets)")
	}
	fmt.Fprintf(w, "quarantined tickets: %d\n", rep.QuarantinedTickets)
	fmt.Fprintf(w, "rejected specs:      %d\n", rep.RejectedSpecs)
	fmt.Fprintf(w, "rejected plans:      %d\n", rep.RejectedPlans)
}
