package channels

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
)

// Ntfy delivers through ntfy.sh or a self-hosted instance.
//
// It is the default channel because it is free, self-hostable, and pushes to a
// phone without an account. A user's destination is a full topic URL, so
// different people can use different servers without extra configuration.
type Ntfy struct {
	client *http.Client
	// defaultServer is used when a destination is a bare topic name rather
	// than a full URL.
	defaultServer string
}

// NewNtfy builds the channel. defaultServer may be empty if every user's
// destination is a full URL.
func NewNtfy(defaultServer string, timeout time.Duration) *Ntfy {
	return &Ntfy{
		client:        newHTTPClient(timeout),
		defaultServer: strings.TrimRight(defaultServer, "/"),
	}
}

func (n *Ntfy) Name() core.Channel { return core.ChannelNtfy }

func (n *Ntfy) Capabilities() notify.Capabilities {
	// ntfy renders an action button from a header, and at max priority it
	// bypasses Do Not Disturb on Android — which is the difference between a
	// notification and a pager.
	return notify.Capabilities{SupportsButtons: true, IsLoud: true}
}

func (n *Ntfy) Send(ctx context.Context, m notify.Message) error {
	url := m.Destination
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		if n.defaultServer == "" {
			return errNoServer
		}
		url = n.defaultServer + "/" + strings.TrimLeft(url, "/")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(m.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	if m.Title != "" {
		req.Header.Set("Title", m.Title)
	}
	req.Header.Set("Priority", ntfyPriority(m.Severity))
	req.Header.Set("Tags", ntfyTags(m.Severity))

	// A tappable acknowledge button, so the page can be answered without
	// opening a browser.
	if m.AckLink != "" {
		req.Header.Set("Actions", "view, Acknowledge, "+m.AckLink+", clear=true")
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return classify(0, "", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classify(resp.StatusCode, string(body), nil)
}

// ntfyPriority maps severity onto ntfy's 1-5 scale.
//
// Critical uses max, which is what bypasses Do Not Disturb. Anything quieter
// would defeat the purpose of a pager.
func ntfyPriority(s core.Severity) string {
	switch s {
	case core.SeverityCritical:
		return "5"
	case core.SeverityWarning:
		return "4"
	case core.SeverityInfo:
		return "3"
	default:
		return "5"
	}
}

func ntfyTags(s core.Severity) string {
	switch s {
	case core.SeverityCritical:
		return "rotating_light"
	case core.SeverityWarning:
		return "warning"
	default:
		return "information_source"
	}
}
