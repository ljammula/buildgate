package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SlackNotifier posts a notification to a single Slack incoming
// webhook. It is a best-effort, out-of-band paging channel alongside
// LogNotifier — the durable audit trail — not a replacement for it.
type SlackNotifier struct {
	WebhookURL string
	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client
}

// slackPayload is Slack's minimal incoming-webhook body shape.
type slackPayload struct {
	Text string `json:"text"`
}

// Notify posts n as a plain-text Slack message. It honors ctx
// cancellation and returns once the HTTP request completes or fails; per
// the Notifier contract, it never waits on a human response.
func (s SlackNotifier) Notify(ctx context.Context, n Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	header := n
	header.Ticket = escapeSlackMentions(n.Ticket)
	text := fmt.Sprintf("factoryd: %s\nreason: %s", subject(header), escapeSlackMentions(n.Reason))
	// Next/Link give the operator one clickable next action instead of
	// leaving them to reconstruct it from the reason text -- Slack's own
	// <url|text> syntax renders a hyperlinked "Next" line when both are
	// present.
	switch {
	case n.Next != "" && n.Link != "":
		text += fmt.Sprintf("\nnext: <%s|%s>", n.Link, escapeSlackMentions(n.Next))
	case n.Next != "":
		text += fmt.Sprintf("\nnext: %s", escapeSlackMentions(n.Next))
	case n.Link != "":
		text += fmt.Sprintf("\n%s", n.Link)
	}

	body, err := json.Marshal(slackPayload{Text: text})
	if err != nil {
		return fmt.Errorf("encode slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build slack request: %w", redactWebhookURL(err))
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post slack webhook: %w", redactWebhookURL(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// escapeSlackMentions defuses Slack's channel-wide (`<!channel>`,
// `<!here>`, `<!everyone>`) and user/group (`<@Uxxxx>`, `<!subteam^...>`)
// mention syntax. Unlike Discord, a plain incoming webhook has no
// allowed_mentions-style suppression field, so a ticket identifier or
// halt reason (CLI-supplied, potentially operator- or generator-
// authored) containing that syntax would otherwise page an entire
// channel or arbitrary users. Slack's own documented escaping (`<` as
// `&lt;`) applied only to the leading `<!`/`<@` breaks the sequence its
// parser requires, rather than stripping the text, so the message still
// shows what was sent.
func escapeSlackMentions(s string) string {
	s = strings.ReplaceAll(s, "<!", "&lt;!")
	s = strings.ReplaceAll(s, "<@", "&lt;@")
	return s
}
