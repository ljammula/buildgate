package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// DiscordNotifier posts a notification to a single Discord incoming
// webhook. It is a best-effort, out-of-band paging channel alongside
// LogNotifier — the durable audit trail — not a replacement for it.
type DiscordNotifier struct {
	WebhookURL string
	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client
}

// discordPayload is Discord's minimal incoming-webhook body shape.
type discordPayload struct {
	Content string `json:"content"`
	// AllowedMentions with every parse type disabled stops the message
	// content from generating real @everyone/@here/user/role mentions.
	// n.Ticket (a CLI-supplied, potentially operator- or generator-
	// authored value) and n.Reason are interpolated into Content below
	// without any escaping, so without this, a ticket identifier
	// containing Discord mention syntax could unexpectedly page an entire
	// channel or arbitrary users/roles.
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

// Notify posts n as a plain-text Discord message. It honors ctx
// cancellation and returns once the HTTP request completes or fails; per
// the Notifier contract, it never waits on a human response.
func (d DiscordNotifier) Notify(ctx context.Context, n Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	content := fmt.Sprintf("factoryd: %s\nreason: %s", subject(n), n.Reason)
	// Next/Link give the operator one next action instead of leaving them
	// to reconstruct it from the reason text. Discord has no hyperlink
	// syntax for a plain webhook message, so Link is appended as a bare
	// URL, which Discord auto-embeds as a clickable link.
	if n.Next != "" {
		content += fmt.Sprintf("\nnext: %s", n.Next)
	}
	if n.Link != "" {
		content += fmt.Sprintf("\n%s", n.Link)
	}

	body, err := json.Marshal(discordPayload{
		Content:         content,
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	})
	if err != nil {
		return fmt.Errorf("encode discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build discord request: %w", redactWebhookURL(err))
	}
	req.Header.Set("Content-Type", "application/json")

	client := d.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post discord webhook: %w", redactWebhookURL(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// redactWebhookURL strips the request URL — which embeds the webhook's
// authentication token — out of an error before it's ever logged. Both
// http.NewRequestWithContext (a malformed URL) and http.Client.Do (a DNS,
// TLS, connection, or timeout failure) return a *url.Error whose Error()
// string includes the full request URL verbatim; logging that error
// as-is would write the live credential into factoryd's logs. Any other
// error type is replaced with a fully generic message, since a caller
// can't prove in advance that some other wrapping type won't also embed
// the URL.
func redactWebhookURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s (webhook URL redacted): %w", uerr.Op, uerr.Err)
	}
	return errors.New("request failed (details redacted to avoid leaking the webhook URL)")
}
