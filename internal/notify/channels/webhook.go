package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
)

// Webhook posts a JSON document to an arbitrary URL.
//
// This is how Slack, Discord and anything else are reached without Kerberon
// growing an integration for each: the operator points it at their own
// endpoint. It is deliberately generic rather than pretending to be a Slack
// app, which would need OAuth and a public callback and would break the
// zero-configuration promise (spec section 8.5).
type Webhook struct {
	client *http.Client
	// defaultURL is used when a user's destination is empty, so a single
	// team-wide webhook can serve everyone.
	defaultURL string
}

// NewWebhook builds the channel.
func NewWebhook(defaultURL string, timeout time.Duration) *Webhook {
	return &Webhook{client: newHTTPClient(timeout), defaultURL: defaultURL}
}

func (w *Webhook) Name() core.Channel { return core.ChannelWebhook }

func (w *Webhook) Capabilities() notify.Capabilities {
	// Whether the far end renders a button is the operator's business, and
	// whether it wakes anyone is unknowable from here.
	return notify.Capabilities{}
}

// WebhookPayload is the document posted. It is a documented shape rather than
// an internal struct, because operators write templates against it.
type WebhookPayload struct {
	IncidentID int64  `json:"incident_id"`
	Severity   string `json:"severity,omitempty"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body"`
	AckURL     string `json:"ack_url,omitempty"`
	// Text duplicates the body under the key Slack and Discord both accept,
	// so a bare incoming-webhook URL works with no transformation.
	Text string `json:"text"`
}

func (w *Webhook) Send(ctx context.Context, m notify.Message) error {
	url := strings.TrimSpace(m.Destination)
	if url == "" {
		url = w.defaultURL
	}
	if url == "" {
		return errNoWebhookURL
	}

	body, err := json.Marshal(WebhookPayload{
		IncidentID: m.IncidentID,
		Severity:   string(m.Severity),
		Title:      m.Title,
		Body:       m.Body,
		AckURL:     m.AckLink,
		Text:       m.Body,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return classify(0, "", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classify(resp.StatusCode, string(respBody), nil)
}
