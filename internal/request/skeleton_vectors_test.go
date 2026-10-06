package request

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/policy"
)

// specSkeletonVectorsPath is the golden file the console's own tests read
// too (console/src/domain/specSkeleton.test.ts). The console mirrors the
// structural checks below to show them while an operator edits; this file
// is what keeps the mirror honest. The inputs are written by hand, the
// expectations by this test:
//
//	FACTORYD_UPDATE_GOLDEN=1 go test ./internal/request -run TestSpecSkeletonGoldenVectors
const specSkeletonVectorsPath = "../../console/test/fixtures/vectors/spec-skeleton.json"

type specSkeletonVector struct {
	Name                 string   `json:"name"`
	Text                 string   `json:"text"`
	Valid                bool     `json:"valid"`
	MissingHeading       string   `json:"missing_heading"`
	CriteriaSectionEmpty bool     `json:"criteria_section_empty"`
	Criteria             []string `json:"criteria"`
}

type ticketSkeletonVector struct {
	Name           string   `json:"name"`
	Text           string   `json:"text"`
	PlanValid      bool     `json:"plan_valid"`
	MissingHeading string   `json:"missing_heading"`
	EmptySection   string   `json:"empty_section"`
	Covered        []int    `json:"covered"`
	HeadersMissing []string `json:"headers_missing"`
}

type planCoverageVector struct {
	Name          string   `json:"name"`
	Spec          string   `json:"spec"`
	Tickets       []string `json:"tickets"`
	CriteriaCount int      `json:"criteria_count"`
	Unclaimed     []int    `json:"unclaimed"`
}

type specSkeletonVectors struct {
	Specs   []specSkeletonVector   `json:"specs"`
	Tickets []ticketSkeletonVector `json:"tickets"`
	Plans   []planCoverageVector   `json:"plans"`
}

// headingNamedBy returns the heading err's message quotes after prefix, or
// "" when err is nil or says something else.
func headingNamedBy(err error, format string, headings []string) string {
	if err == nil {
		return ""
	}
	for _, h := range headings {
		if strings.HasPrefix(err.Error(), fmt.Sprintf(format, h)) {
			return h
		}
	}
	return ""
}

func specSkeletonOutcome(v specSkeletonVector) specSkeletonVector {
	err := ValidateSpecSkeleton(v.Text)
	criteria, criteriaErr := SpecAcceptanceCriteria(v.Text)
	if criteriaErr != nil || criteria == nil {
		criteria = []string{}
	}
	return specSkeletonVector{
		Name:                 v.Name,
		Text:                 v.Text,
		Valid:                err == nil,
		MissingHeading:       headingNamedBy(err, "spec is missing required heading %q", requiredSpecHeadings),
		CriteriaSectionEmpty: err != nil && strings.HasSuffix(err.Error(), "section has no content"),
		Criteria:             criteria,
	}
}

func ticketSkeletonOutcome(v ticketSkeletonVector) ticketSkeletonVector {
	err := ValidateTicketPlan(v.Text)
	covered, coveredErr := TicketCoveredCriteria(v.Text)
	if coveredErr != nil || covered == nil {
		covered = []int{}
	}
	headers := []string{}
	_, reasons := policy.TicketStructureBrownfield(v.Text)
	for _, reason := range reasons {
		if key, ok := strings.CutPrefix(reason, "ticket is missing required header line "); ok {
			headers = append(headers, key)
		}
	}
	empty := ""
	if err != nil && strings.HasSuffix(err.Error(), "section has no content") {
		empty = headingNamedBy(err, "ticket's %q section has no content", requiredTicketHeadings)
	}
	return ticketSkeletonVector{
		Name:           v.Name,
		Text:           v.Text,
		PlanValid:      err == nil,
		MissingHeading: headingNamedBy(err, "ticket is missing required heading %q", requiredTicketHeadings),
		EmptySection:   empty,
		Covered:        covered,
		HeadersMissing: headers,
	}
}

// planCoverageOutcome reads the unclaimed numbers back out of
// ValidatePlanCoverage's own message, so the vector records what that
// function decided and not a second computation of it.
func planCoverageOutcome(t *testing.T, v planCoverageVector) planCoverageVector {
	t.Helper()
	count, err := SpecAcceptanceCriteriaCount(v.Spec)
	if err != nil {
		count = 0
	}
	unclaimed := []int{}
	if err := ValidatePlanCoverage(count, v.Tickets); err != nil {
		list, ok := strings.CutPrefix(err.Error(), "acceptance criteria claimed by no ticket: ")
		if !ok {
			t.Fatalf("plan %q: unexpected coverage error %q", v.Name, err)
		}
		for _, field := range strings.Fields(strings.Trim(list, "[]")) {
			n, err := strconv.Atoi(field)
			if err != nil {
				t.Fatalf("plan %q: %v", v.Name, err)
			}
			unclaimed = append(unclaimed, n)
		}
	}
	tickets := v.Tickets
	if tickets == nil {
		tickets = []string{}
	}
	return planCoverageVector{Name: v.Name, Spec: v.Spec, Tickets: tickets, CriteriaCount: count, Unclaimed: unclaimed}
}

// TestSpecSkeletonGoldenVectors pins what the spec and ticket structure
// checks decide for each vector, so the console's mirror of them is tested
// against the server's own answers.
func TestSpecSkeletonGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(specSkeletonVectorsPath)
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var want specSkeletonVectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode golden vectors: %v", err)
	}
	if len(want.Specs) == 0 || len(want.Tickets) == 0 || len(want.Plans) == 0 {
		t.Fatalf("golden vectors are missing a section: %d specs, %d tickets, %d plans", len(want.Specs), len(want.Tickets), len(want.Plans))
	}
	var got specSkeletonVectors
	for _, v := range want.Specs {
		got.Specs = append(got.Specs, specSkeletonOutcome(v))
	}
	for _, v := range want.Tickets {
		got.Tickets = append(got.Tickets, ticketSkeletonOutcome(v))
	}
	for _, v := range want.Plans {
		got.Plans = append(got.Plans, planCoverageOutcome(t, v))
	}

	if os.Getenv("FACTORYD_UPDATE_GOLDEN") == "1" {
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(specSkeletonVectorsPath, out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i, v := range got.Specs {
		if !reflect.DeepEqual(v, want.Specs[i]) {
			t.Errorf("spec %q: got %+v, want %+v", v.Name, v, want.Specs[i])
		}
	}
	for i, v := range got.Tickets {
		if !reflect.DeepEqual(v, want.Tickets[i]) {
			t.Errorf("ticket %q: got %+v, want %+v", v.Name, v, want.Tickets[i])
		}
	}
	for i, v := range got.Plans {
		if !reflect.DeepEqual(v, want.Plans[i]) {
			t.Errorf("plan %q: got %+v, want %+v", v.Name, v, want.Plans[i])
		}
	}
}
