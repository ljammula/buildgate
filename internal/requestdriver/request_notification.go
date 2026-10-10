package requestdriver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/notify"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// requestNotice is what one notification about a request says.
type requestNotice struct {
	// Ask is the headline: what is asked of the operator.
	Ask string
	// Detail is the sentence under it.
	Detail string
	// Next is the command or URL that acts on it, for a reader with no
	// console (the log, Slack, Discord).
	Next string
}

// maxSubjectTitleRunes caps the request title in a notification's Subject.
const maxSubjectTitleRunes = 120

// requestSubject names r as the console does: its project and its title.
func requestSubject(dataDir string, r *request.Request) string {
	title := []rune(request.Title(dataDir, r.ID))
	if len(title) > maxSubjectTitleRunes {
		title = append(title[:maxSubjectTitleRunes], '…')
	}
	switch {
	case len(title) == 0:
		return r.Project
	case r.Project == "":
		return string(title)
	}
	return r.Project + ": " + string(title)
}

// prepareRequestNotification builds the notification that tells the operator
// r waits on them, appends it to r's notifications.log, and records it on r
// (LastNotifiedAt, LastAsk: what an open console tab raises its own
// notification from). It does not save r and does not dispatch: the caller
// does both, in the order its own step needs (notify.DispatchExternal).
//
// The request's title (the operator's text, or a GitHub issue's) and the
// detail (a halt reason can quote a build's output) are made one line of
// plain text first: they reach a banner, an AppleScript string and a webhook.
//
// The link is r's own page in the console, and for a channel read elsewhere
// the same page at the remote console's address. Neither carries a token.
func prepareRequestNotification(dataDir string, r *request.Request, notice requestNotice, now time.Time) notify.Notification {
	ts := now.UTC().Format(time.RFC3339Nano)
	delivered := true
	n := notify.Notification{
		RequestID:  r.ID,
		Ask:        notice.Ask,
		Subject:    sanitize.Line(requestSubject(dataDir, r)),
		Reason:     sanitize.Line(notice.Detail),
		State:      run.State(r.State),
		SentAt:     ts,
		Delivered:  &delivered,
		Next:       notice.Next,
		Link:       consolelink.RequestURL(consolelink.BaseURL("", dataDir), r.ID),
		RemoteLink: consolelink.RequestURL(consolelink.RemoteBaseURL(dataDir), r.ID),
	}
	// request.Dir exists since submit; nothing here depends on that.
	if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err == nil {
		notifyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		notifyErr := (notify.LogNotifier{Path: RequestNotificationLogPath(dataDir, r.ID)}).Notify(notifyCtx, n)
		cancel()
		if notifyErr != nil {
			delivered = false
			n.DeliveryError = notifyErr.Error()
		}
	}
	r.LastNotifiedAt = ts
	r.LastAsk = notice.Ask
	return n
}

// reviewNotice is the notice for a request waiting in a review state.
// waited is how long it has waited; zero on the first notification.
func reviewNotice(dataDir string, r *request.Request, waited time.Duration) requestNotice {
	approve := fmt.Sprintf("factoryd approve %s (or `factoryd reject -reason ... %s`)", r.ID, r.ID)
	var notice requestNotice
	switch r.State {
	case request.StateOracleReview:
		notice = requestNotice{Ask: "Acceptance tests ready for your review", Detail: "Read them, then approve, edit or skip them.", Next: approve}
	case request.StatePlanReview:
		detail := "Read the tickets, then approve or request changes."
		if n := len(r.Tickets); n > 0 {
			detail = fmt.Sprintf("%d ticket(s). %s", n, detail)
		}
		notice = requestNotice{Ask: "Plan ready for your review", Detail: detail + PlanReviewOracleNote(dataDir, r), Next: approve}
	case request.StateResumeReview:
		// Nothing to edit or approve: a step was lost, and the ways on are
		// the resume verbs (NextAction names the lost state).
		notice = requestNotice{
			Ask:    "A step was lost: choose how to continue",
			Detail: r.NextAction(),
			Next:   fmt.Sprintf("factoryd resume %s (or `-from scratch`, or `factoryd cancel %s`)", r.ID, r.ID),
		}
	default: // request.StateSpecReview, and any state that ends up here.
		notice = requestNotice{Ask: "Spec ready for your review", Detail: "Read it, then approve or request changes.", Next: approve}
	}
	if note := request.OracleReviewNotice(dataDir, r); note != "" {
		notice.Detail += " -- NOTE: " + note
	}
	if waited >= time.Minute {
		notice.Detail = fmt.Sprintf("Waiting for you for %s. %s", waited.Round(time.Minute), notice.Detail)
	}
	return notice
}

// terminalNotice is the notice for a request that has just halted or been
// quarantined with reason.
func terminalNotice(r *request.Request, reason string) requestNotice {
	ask := "Halted: needs you"
	if r.State == request.StateQuarantined {
		ask = "Quarantined: needs you"
	}
	return requestNotice{Ask: ask, Detail: reason, Next: fmt.Sprintf("factoryd retry %s", r.ID)}
}

// The two things a ticket's pull request can ask of the operator.
const (
	pullRequestAskReview = "review"
	pullRequestAskMerge  = "merge"
)

// notifyPullRequestWaiting tells the operator, once, that ticket's pull
// request waits on them: when it is ready to merge (its MergeReadiness
// check passed), or ready for review and not yet ready to merge. For one
// head it sends at most one of each, and never "review" after "merge": a
// thread the operator opens on a pull request they were told to merge is
// not news to them. It records what it sent on ticket and r; the caller
// saves r.
func notifyPullRequestWaiting(dataDir string, r *request.Request, ticket *request.Ticket, now time.Time) {
	mr := ticket.MergeReadiness
	if mr == nil || ticket.PRURL == "" {
		return
	}
	var notice requestNotice
	var ask string
	switch {
	case mr.Ready && (ticket.PRState == "ready" || ticket.PRState == "approved"):
		ask = pullRequestAskMerge
		notice = requestNotice{
			Ask:    "Pull request ready to merge",
			Detail: fmt.Sprintf("%s: checks pass, no review thread is open, and the last code review of the whole diff is clean. The factory never merges.", ticket.PRURL),
		}
	case ticket.PRState == "ready":
		ask = pullRequestAskReview
		notice = requestNotice{
			Ask:    "Pull request ready for your review",
			Detail: fmt.Sprintf("%s is not ready to merge yet: %s.", ticket.PRURL, strings.Join(mr.Blockers, "; ")),
		}
	default:
		return
	}
	sent := ask + ":" + mr.HeadSHA
	if ticket.NotifiedPR == sent || ticket.NotifiedPR == pullRequestAskMerge+":"+mr.HeadSHA {
		return
	}
	if r.TicketCount > 1 {
		notice.Detail = fmt.Sprintf("Ticket %d/%d. %s", ticket.Index, r.TicketCount, notice.Detail)
	}
	notice.Next = ticket.PRURL
	n := prepareRequestNotification(dataDir, r, notice, now)
	ticket.NotifiedPR = sent
	notify.DispatchExternal(dataDir, n)
}
