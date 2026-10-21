package requestdriver

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"buildgate/internal/request"
)

// specRelPath is spec.md's request-relative path, the key of a RedraftScope.
const specRelPath = "spec.md"

// spliceScopedSpec applies the review's comments to a redrafted spec: when the
// newest spec_review rejection commented on sections (request.RedraftScope),
// the result is the reviewed spec.md with only those sections taken from
// drafted, every other byte copied from the file on disk (hand edits
// included). The model writes a whole document; this, not the model, decides
// what of it lands. Any other redraft returns drafted as is. An error means
// the draft lacks a commented section: a structural refusal.
func spliceScopedSpec(dataDir string, r *request.Request, drafted string) (string, error) {
	scope := r.RedraftScope(request.StateSpecReview)
	if scope == nil || importsSpec(r) {
		return drafted, nil
	}
	headings, ok := scope.Sections(specRelPath)
	if !ok || scope.Whole(specRelPath) {
		return drafted, nil
	}
	current, err := os.ReadFile(RequestSpecPath(dataDir, r.ID))
	if err != nil {
		// Nothing reviewed on disk to keep: a full redraft stands.
		return drafted, nil
	}
	return request.SpliceSections(string(current), drafted, request.SpecSkeletonHeadings(), headings)
}

// spliceScopedTickets is spliceScopedSpec for a plan: current is the reviewed
// tickets read before the planning job ran. A ticket nobody commented on keeps
// its current bytes whatever the model wrote; a ticket commented as a whole
// takes the redraft's file of the same name; a ticket with commented sections
// takes only those sections. Tickets the model added or dropped without a
// comment are ignored, so the plan's split cannot change under a scoped
// review. A commented ticket the redraft lacks is a content error (one
// automatic re-plan).
func spliceScopedTickets(r *request.Request, current, drafted []DraftedTicket) ([]DraftedTicket, error) {
	scope := r.RedraftScope(request.StatePlanReview)
	if scope == nil || importsPlan(r) || len(current) == 0 {
		return drafted, nil
	}
	byName := make(map[string]string, len(drafted))
	for _, t := range drafted {
		byName[t.Filename] = t.Content
	}
	out := make([]DraftedTicket, 0, len(current))
	for _, t := range current {
		headings, commented := scope.Sections(path.Join("tickets", t.Filename))
		if !commented {
			out = append(out, t)
			continue
		}
		draft, ok := byName[t.Filename]
		if !ok {
			return nil, contentErr(fmt.Errorf("ticket %s was commented on, and the redraft has no file of that name", t.Filename))
		}
		if headings == nil {
			out = append(out, DraftedTicket{Filename: t.Filename, Content: draft})
			continue
		}
		content, err := request.SpliceSections(t.Content, draft, request.TicketSkeletonHeadings(), headings)
		if err != nil {
			return nil, contentErr(fmt.Errorf("ticket %s: %w", t.Filename, err))
		}
		out = append(out, DraftedTicket{Filename: t.Filename, Content: content})
	}
	return out, nil
}

// ReviewedTickets returns the tickets a scoped plan redraft revises: the
// plan_review rejection's snapshot of them, which stays whatever happens to
// tickets/ during the redraft. Nil, nil when the redraft is not scoped (or
// the plan was handed over). A scoped redraft with no reviewed tickets to keep
// is an error: it must not quietly become a full redraft.
func ReviewedTickets(dataDir string, r *request.Request) ([]DraftedTicket, error) {
	if r.RedraftScope(request.StatePlanReview) == nil || importsPlan(r) {
		return nil, nil
	}
	var tickets []DraftedTicket
	for path, content := range request.ReviewedFiles(dataDir, r, request.StatePlanReview) {
		if name, ok := strings.CutPrefix(path, "tickets/"); ok && strings.HasSuffix(name, ".spec.md") && !strings.Contains(name, "/") {
			tickets = append(tickets, DraftedTicket{Filename: name, Content: content})
		}
	}
	if len(tickets) == 0 {
		return nil, fmt.Errorf("the plan review commented on passages, but the tickets it reviewed are not kept with the request: send the plan back for a whole redraft with `factoryd reject -to plan -reason \"...\" %s`", r.ID)
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].Filename < tickets[j].Filename })
	return tickets, nil
}

// writeAndValidateScopedTickets is writeAndValidateDraftedTickets over the
// plan a scoped redraft leaves: reviewed with the commented sections replaced
// (spliceScopedTickets). Every check runs on the spliced set, exactly as on a
// full redraft.
func writeAndValidateScopedTickets(ticketsDir string, r *request.Request, reviewed, drafted []DraftedTicket, criteriaCount int, verifyCommand string, testPatterns []string) ([]request.Ticket, error) {
	spliced, err := spliceScopedTickets(r, reviewed, drafted)
	if err != nil {
		return nil, err
	}
	return writeAndValidateDraftedTickets(ticketsDir, spliced, criteriaCount, verifyCommand, testPatterns)
}
