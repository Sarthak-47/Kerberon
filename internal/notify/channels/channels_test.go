package channels_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
	"github.com/Sarthak-47/kerberon/internal/notify/channels"
)

func msg() notify.Message {
	return notify.Message{
		IncidentID:  42,
		Destination: "",
		Title:       "API is down",
		Body:        "[critical] API is down\nteam: platform",
		Severity:    core.SeverityCritical,
		AckLink:     "https://k.example.com/ack/42/sarthak/tok",
	}
}

// ─── ntfy ─────────────────────────────────────────────────────────────────

func TestNtfyPostsTheBodyWithPriorityAndAction(t *testing.T) {
	var (
		gotBody   string
		gotHeader http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := msg()
	m.Destination = srv.URL + "/kerberon-sarthak"

	if err := channels.NewNtfy("", time.Second).Send(context.Background(), m); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotBody != m.Body {
		t.Errorf("body = %q", gotBody)
	}
	if got := gotHeader.Get("Title"); got != "API is down" {
		t.Errorf("Title = %q", got)
	}
	// Max priority is what bypasses Do Not Disturb, which is the difference
	// between a notification and a pager.
	if got := gotHeader.Get("Priority"); got != "5" {
		t.Errorf("Priority = %q, want 5 for a critical incident", got)
	}
	if got := gotHeader.Get("Actions"); !strings.Contains(got, m.AckLink) {
		t.Errorf("Actions = %q, want an acknowledge action carrying the ack link", got)
	}
}

func TestNtfyPriorityFollowsSeverity(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Priority"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := channels.NewNtfy("", time.Second)
	for _, sev := range []core.Severity{core.SeverityCritical, core.SeverityWarning, core.SeverityInfo} {
		m := msg()
		m.Destination = srv.URL + "/t"
		m.Severity = sev
		if err := ch.Send(context.Background(), m); err != nil {
			t.Fatalf("send %s: %v", sev, err)
		}
	}
	if len(got) != 3 || got[0] != "5" || got[1] != "4" || got[2] != "3" {
		t.Errorf("priorities = %v, want 5, 4, 3", got)
	}
}

func TestNtfyUsesTheDefaultServerForABareTopic(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := msg()
	m.Destination = "kerberon-sarthak"

	if err := channels.NewNtfy(srv.URL, time.Second).Send(context.Background(), m); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotPath != "/kerberon-sarthak" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestNtfyBareTopicWithoutADefaultServerIsNotRetryable(t *testing.T) {
	m := msg()
	m.Destination = "bare-topic"

	err := channels.NewNtfy("", time.Second).Send(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if notify.IsRetryable(err) {
		t.Error("a configuration mistake should not be retried")
	}
}

// ─── Failure classification ───────────────────────────────────────────────

// Retrying something that cannot succeed burns the window in which the
// incident could still have been pursued by another channel.
func TestHTTPStatusClassification(t *testing.T) {
	cases := []struct {
		status    int
		wantErr   bool
		retryable bool
		why       string
	}{
		{200, false, false, "success"},
		{204, false, false, "success with no content"},
		{400, true, false, "a malformed request will not fix itself"},
		{401, true, false, "a bad token will not fix itself"},
		{404, true, false, "a missing topic will not fix itself"},
		{429, true, true, "rate limiting is the provider asking us to slow down"},
		{500, true, true, "the provider is broken, not the request"},
		{502, true, true, "a bad gateway is transient"},
		{503, true, true, "unavailable is transient"},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			m := msg()
			m.Destination = srv.URL + "/t"
			err := channels.NewNtfy("", time.Second).Send(context.Background(), m)

			if c.wantErr && err == nil {
				t.Fatalf("status %d should be an error (%s)", c.status, c.why)
			}
			if !c.wantErr {
				if err != nil {
					t.Fatalf("status %d: %v", c.status, err)
				}
				return
			}
			if got := notify.IsRetryable(err); got != c.retryable {
				t.Errorf("status %d retryable = %v, want %v (%s)", c.status, got, c.retryable, c.why)
			}
		})
	}
}

// A provider that hangs must not hold a worker while an incident goes unpaged.
func TestTimeoutIsRetryable(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	m := msg()
	m.Destination = srv.URL + "/t"

	err := channels.NewNtfy("", 50*time.Millisecond).Send(context.Background(), m)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !notify.IsRetryable(err) {
		t.Errorf("a timeout should be retryable, got %v", err)
	}
}

// ─── Telegram ─────────────────────────────────────────────────────────────

func TestTelegramSendsAnInlineAcknowledgeButton(t *testing.T) {
	var payload struct {
		ChatID      string `json:"chat_id"`
		Text        string `json:"text"`
		ReplyMarkup *struct {
			InlineKeyboard [][]struct {
				Text string `json:"text"`
				URL  string `json:"url"`
			} `json:"inline_keyboard"`
		} `json:"reply_markup"`
	}
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	m := msg()
	m.Destination = "123456789"

	ch := channels.NewTelegram("bot-token", time.Second).WithAPIBase(srv.URL)
	if err := ch.Send(context.Background(), m); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotPath != "/botbot-token/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	if payload.ChatID != "123456789" {
		t.Errorf("chat_id = %q", payload.ChatID)
	}
	if payload.ReplyMarkup == nil || len(payload.ReplyMarkup.InlineKeyboard) == 0 {
		t.Fatal("no inline keyboard; the point of Telegram is the button")
	}
	btn := payload.ReplyMarkup.InlineKeyboard[0][0]
	if btn.URL != m.AckLink {
		t.Errorf("button URL = %q, want the ack link", btn.URL)
	}
}

// Telegram answers 200 with ok:false for problems like a chat the bot cannot
// reach, so the status code alone would silently swallow the failure.
func TestTelegramOkFalseIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":false,"description":"chat not found"}`)
	}))
	defer srv.Close()

	m := msg()
	m.Destination = "999"
	ch := channels.NewTelegram("tok", time.Second).WithAPIBase(srv.URL)

	err := ch.Send(context.Background(), m)
	if err == nil {
		t.Fatal("a 200 with ok:false was treated as success")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error should carry the provider's reason: %v", err)
	}
}

func TestTelegramWithoutATokenIsNotRetryable(t *testing.T) {
	err := channels.NewTelegram("", time.Second).Send(context.Background(), msg())
	if err == nil {
		t.Fatal("expected an error")
	}
	if notify.IsRetryable(err) {
		t.Error("a missing bot token is a configuration mistake, not a transient fault")
	}
}

// ─── Webhook ──────────────────────────────────────────────────────────────

func TestWebhookPostsADocumentedPayload(t *testing.T) {
	var got channels.WebhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := msg()
	m.Destination = srv.URL

	if err := channels.NewWebhook("", time.Second).Send(context.Background(), m); err != nil {
		t.Fatalf("send: %v", err)
	}

	if got.IncidentID != 42 || got.Body != m.Body || got.AckURL != m.AckLink {
		t.Errorf("payload = %+v", got)
	}
	// text duplicates the body under the key Slack and Discord both accept,
	// so a bare incoming-webhook URL works with no transformation.
	if got.Text != m.Body {
		t.Errorf("text = %q, want the body duplicated for Slack-compatible endpoints", got.Text)
	}
}

func TestWebhookFallsBackToTheDefaultURL(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := msg()
	m.Destination = ""

	if err := channels.NewWebhook(srv.URL, time.Second).Send(context.Background(), m); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !hit {
		t.Error("the default URL was not used")
	}
}

func TestWebhookWithNoURLAtAllIsNotRetryable(t *testing.T) {
	m := msg()
	m.Destination = ""
	err := channels.NewWebhook("", time.Second).Send(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if notify.IsRetryable(err) {
		t.Error("having nowhere to post is a configuration mistake")
	}
}

// ─── Capabilities ─────────────────────────────────────────────────────────

// A policy should not be able to look louder than it is.
func TestCapabilitiesReflectReality(t *testing.T) {
	ntfy := channels.NewNtfy("", time.Second)
	if !ntfy.Capabilities().IsLoud || !ntfy.Capabilities().SupportsButtons {
		t.Error("ntfy is loud and renders an action button")
	}

	tg := channels.NewTelegram("t", time.Second)
	if !tg.Capabilities().SupportsButtons {
		t.Error("telegram renders an inline button")
	}

	email := channels.NewEmail(channels.EmailConfig{Host: "smtp.example.com"})
	if email.Capabilities().IsLoud {
		t.Error("email waits until morning; calling it loud would let a policy overstate itself")
	}
}

func TestChannelNames(t *testing.T) {
	cases := map[core.Channel]notify.Channel{
		core.ChannelNtfy:     channels.NewNtfy("", time.Second),
		core.ChannelTelegram: channels.NewTelegram("t", time.Second),
		core.ChannelWebhook:  channels.NewWebhook("u", time.Second),
		core.ChannelEmail:    channels.NewEmail(channels.EmailConfig{Host: "h"}),
	}
	for want, ch := range cases {
		if got := ch.Name(); got != want {
			t.Errorf("name = %q, want %q", got, want)
		}
	}
}
