package request

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scopedSpec is a spec with every required heading, written out literally so
// a test knows each section's bytes without asking the code under test.
var scopedSpecSections = []string{
	"# Spec\n\nTitle text.\n\n",
	"## Problem\n\nRefunds double-process.\n\n",
	"## Scope\n\nThe refund path.\n\n### Detail\n\nA subsection that is not a skeleton heading.\n\n",
	"## Non-goals\n\nNothing else.\n\n",
	"## Affected services and packages\n\n- billing\n\n",
	"## Acceptance criteria\n\n1. A repeat is refused.\n2. The first is kept.\n\n",
	"## Risks\n\nNone.\n\n",
	"## Open questions\n\nNone.\n",
}

func joinSections(sections []string) string { return strings.Join(sections, "") }

// rewriteEverySection is a draft that changed every section.
func rewriteEverySection(sections []string) string {
	out := make([]string, len(sections))
	for i, s := range sections {
		heading, _, _ := strings.Cut(s, "\n")
		out[i] = heading + "\n\nREWRITTEN " + heading + "\n\n"
	}
	return joinSections(out)
}

func TestSpliceSectionsKeepsUncommentedSectionsByteIdentical(t *testing.T) {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	withCRLF := make([]string, len(scopedSpecSections))
	for i, s := range scopedSpecSections {
		withCRLF[i] = crlf(s)
	}
	cases := map[string]struct {
		sections []string
		preamble string
	}{
		"lf":                {sections: scopedSpecSections},
		"crlf":              {sections: withCRLF},
		"preamble":          {sections: scopedSpecSections, preamble: "<!-- hand note -->\n\n"},
		"no final newline":  {sections: append(append([]string(nil), scopedSpecSections[:7]...), strings.TrimSuffix(scopedSpecSections[7], "\n"))},
		"heading with tabs": {sections: append([]string{"  # Spec  \n\nT.\n\n"}, scopedSpecSections[1:]...)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			current := tc.preamble + joinSections(tc.sections)
			drafted := rewriteEverySection(scopedSpecSections)
			take := []string{"## Scope", "## Risks"}
			got, err := SpliceSections(current, drafted, SpecSkeletonHeadings(), take)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got, tc.preamble) {
				t.Errorf("the text before the first heading changed: %q", got)
			}
			for _, section := range tc.sections {
				heading := strings.TrimSpace(strings.SplitN(section, "\n", 2)[0])
				text, ok := SectionText(got, SpecSkeletonHeadings(), heading)
				if !ok {
					t.Fatalf("section %q is gone", heading)
				}
				if heading == "## Scope" || heading == "## Risks" {
					draftedText, _ := SectionText(drafted, SpecSkeletonHeadings(), heading)
					if text != draftedText {
						t.Errorf("%s = %q, want the redraft's %q", heading, text, draftedText)
					}
					continue
				}
				if !bytes.Equal([]byte(text), []byte(section)) {
					t.Errorf("%s changed:\n got %q\nwant %q", heading, text, section)
				}
			}
		})
	}
}

func TestSpliceSectionsRefusesADraftMissingACommentedHeading(t *testing.T) {
	current := joinSections(scopedSpecSections)
	drafted := strings.Replace(rewriteEverySection(scopedSpecSections), "## Risks", "## Hazards", 1)
	if _, err := SpliceSections(current, drafted, SpecSkeletonHeadings(), []string{"## Risks"}); err == nil || !strings.Contains(err.Error(), "## Risks") {
		t.Errorf("err = %v, want one naming the missing section", err)
	}
	if _, err := SpliceSections(current, rewriteEverySection(scopedSpecSections), SpecSkeletonHeadings(), []string{"## Nope"}); err == nil {
		t.Error("a section the skeleton lacks: want an error")
	}
	// A draft that lost a section nobody commented on is no concern of the splice.
	if _, err := SpliceSections(current, drafted, SpecSkeletonHeadings(), []string{"## Scope"}); err != nil {
		t.Errorf("an uncommented section missing from the draft: %v", err)
	}
}

func writeSpecFile(t *testing.T, dataDir, id, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(Dir(dataDir, id), specFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRejectRefusesACommentWhosePassageIsNotInItsSection(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)
	writeSpecFile(t, dataDir, "req-1", joinSections(scopedSpecSections))
	cases := map[string]RejectionAnchor{
		"quote in another section": {Path: "spec.md", Section: "## Scope", Quote: "Refunds double-process.", Note: "n"},
		"quote nowhere":            {Path: "spec.md", Quote: "not in the file", Note: "n"},
		"item past the criteria":   {Path: "spec.md", Section: "## Acceptance criteria", Item: 3, Note: "n"},
	}
	for name, anchor := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := RejectAnchored(dataDir, "req-1", "bob", "", []RejectionAnchor{anchor}, fixedNow)
			if !errors.Is(err, ErrPassageMoved) || !strings.Contains(err.Error(), "comment 1") {
				t.Fatalf("err = %v, want ErrPassageMoved naming comment 1", err)
			}
			if r, _ := Load(dataDir, "req-1"); r.State != StateSpecReview || len(r.Rejections) != 0 {
				t.Errorf("a refused rejection changed the request: %+v", r)
			}
		})
	}
	ok := RejectionAnchor{Path: "spec.md", Section: "## Scope", Quote: "The refund path.", Note: "n"}
	if _, err := RejectAnchored(dataDir, "req-1", "bob", "", []RejectionAnchor{ok}, fixedNow); err != nil {
		t.Errorf("a quote inside its section: %v", err)
	}
}

func TestRejectRefusesACommentOnAMissingHeadingOrTicket(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)
	writeSpecFile(t, dataDir, "req-1", joinSections(scopedSpecSections))
	for name, anchor := range map[string]RejectionAnchor{
		"missing heading":  {Path: "spec.md", Section: "## Rollout", Note: "n"},
		"a ticket at spec": {Path: "tickets/001.spec.md", Note: "n"},
		"other file":       {Path: "README.md", Note: "n"},
	} {
		if _, err := RejectAnchored(dataDir, "req-1", "bob", "", []RejectionAnchor{anchor}, fixedNow); !errors.Is(err, ErrPassageMoved) {
			t.Errorf("%s: err = %v, want ErrPassageMoved", name, err)
		}
	}
	planDir := t.TempDir()
	newApprovableRequest(t, planDir, "req-2", StatePlanReview, true)
	for name, anchor := range map[string]RejectionAnchor{
		"missing ticket":  {Path: "tickets/009.spec.md", Note: "n"},
		"missing section": {Path: "tickets/001.spec.md", Section: "### Rollback", Note: "n"},
		"spec at plan":    {Path: "spec.md", Note: "n"},
		"escaping path":   {Path: "tickets/../spec.md", Note: "n"},
	} {
		if _, err := RejectAnchored(planDir, "req-2", "bob", "", []RejectionAnchor{anchor}, fixedNow); !errors.Is(err, ErrPassageMoved) {
			t.Errorf("%s: err = %v, want ErrPassageMoved", name, err)
		}
	}
	good := RejectionAnchor{Path: "tickets/001.spec.md", Section: "### Steps", Note: "n"}
	if _, err := RejectAnchored(planDir, "req-2", "bob", "", []RejectionAnchor{good}, fixedNow); err != nil {
		t.Errorf("a comment on an existing ticket section: %v", err)
	}
}

func rejectionWith(by string, forStage State, note string, anchors ...RejectionAnchor) Rejection {
	return Rejection{By: by, FromState: StateSpecReview, ForStage: forStage, Anchors: anchors, Note: note, Reason: "x"}
}

func TestRedraftScopeIgnoresFactoryRejectionsAndSendBacks(t *testing.T) {
	scope := RejectionAnchor{Path: "spec.md", Section: "## Scope", Note: "n"}
	r := &Request{Rejections: []Rejection{rejectionWith("bob", "", "", scope)}}
	got := r.RedraftScope(StateSpecReview)
	if got == nil || len(got.Files["spec.md"]) != 1 || got.Files["spec.md"][0] != "## Scope" {
		t.Fatalf("scope = %+v", got)
	}
	// The factory's own re-plan rejection neither widens nor narrows it.
	r.Rejections = append(r.Rejections, Rejection{By: FactoryActor, FromState: StatePlanning, ForStage: StatePlanReview, Reason: "x"})
	if r.RedraftScope(StateSpecReview) == nil {
		t.Error("a factory rejection of another stage dropped the scope")
	}
	planReq := &Request{Rejections: []Rejection{
		{By: "bob", FromState: StatePlanReview, Reason: "x", Anchors: []RejectionAnchor{{Path: "tickets/001.spec.md", Section: "## Plan", Note: "n"}}},
		{By: FactoryActor, FromState: StatePlanning, ForStage: StatePlanReview, Reason: "infeasible"},
	}}
	plan := planReq.RedraftScope(StatePlanReview)
	if plan == nil || len(plan.Files["tickets/001.spec.md"]) != 5 {
		t.Fatalf("## Plan expands to itself and its four parts: %+v", plan)
	}
	// A send-back is the newest feedback and names no passage.
	r.Rejections = append(r.Rejections, rejectionWith("bob", StateSpecReview, ""))
	if r.RedraftScope(StateSpecReview) != nil {
		t.Error("a send-back after a scoped rejection must make a full redraft")
	}
	if (&Request{}).RedraftScope(StateSpecReview) != nil {
		t.Error("no rejection: want a full redraft")
	}
}

func TestAWholeSpecCommentMakesAFullRedraft(t *testing.T) {
	whole := RejectionAnchor{Path: "spec.md", Note: "start over"}
	scope := RejectionAnchor{Path: "spec.md", Section: "## Scope", Note: "n"}
	for name, rej := range map[string]Rejection{
		"whole-spec anchor": rejectionWith("bob", "", "", scope, whole),
		"free note beside":  rejectionWith("bob", "", "also the tone", scope),
		"no anchors":        rejectionWith("bob", "", ""),
		"unknown section":   rejectionWith("bob", "", "", RejectionAnchor{Path: "spec.md", Section: "## Rollout", Note: "n"}),
	} {
		if (&Request{Rejections: []Rejection{rej}}).RedraftScope(StateSpecReview) != nil {
			t.Errorf("%s: want a full redraft", name)
		}
	}
	ticket := (&Request{Rejections: []Rejection{{By: "bob", FromState: StatePlanReview, Reason: "x",
		Anchors: []RejectionAnchor{{Path: "tickets/002.spec.md", Note: "n"}}}}}).RedraftScope(StatePlanReview)
	if ticket == nil || !ticket.Whole("tickets/002.spec.md") {
		t.Errorf("a whole-ticket comment scopes that ticket whole: %+v", ticket)
	}
}
