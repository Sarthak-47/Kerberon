package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
)

// Telegram delivers through the Bot API.
//
// It is worth carrying despite needing a bot token because it renders a real
// inline button: the page can be acknowledged from the chat without opening a
// browser, which matters at 3am.
type Telegram struct {
	client   *http.Client
	botToken string
	apiBase  string
}

// NewTelegram builds the channel.
func NewTelegram(botToken string, timeout time.Duration) *Telegram {
	return &Telegram{
		client:   newHTTPClient(timeout),
		botToken: botToken,
		apiBase:  "https://api.telegram.org",
	}
}

// WithAPIBase points the channel at a different host, for tests.
func (t *Telegram) WithAPIBase(base string) *Telegram {
	t.apiBase = base
	return t
}

func (t *Telegram) Name() core.Channel { return core.ChannelTelegram }

func (t *Telegram) Capabilities() notify.Capabilities {
	return notify.Capabilities{SupportsButtons: true, IsLoud: true}
}

type telegramRequest struct {
	ChatID      string               `json:"chat_id"`
	Text        string               `json:"text"`
	ReplyMarkup *telegramReplyMarkup `json:"reply_markup,omitempty"`
}

type telegramReplyMarkup struct {
	InlineKeyboard [][]telegramButton `json:"inline_keyboard"`
}

type telegramButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

func (t *Telegram) Send(ctx context.Context, m notify.Message) error {
	if t.botToken == "" {
		return errNoBotToken
	}

	payload := telegramRequest{
		ChatID: m.Destination,
		Text:   m.Body,
	}
	if m.AckLink != "" {
		payload.ReplyMarkup = &telegramReplyMarkup{
			InlineKeyboard: [][]telegramButton{{
				{Text: "Acknowledge", URL: m.AckLink},
			}},
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", t.apiBase, t.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return classify(0, "", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// Telegram answers 200 with ok:false for application-level problems such
	// as a chat the bot cannot reach, so the status alone is not enough.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(respBody, &out); err == nil && !out.OK {
			return fmt.Errorf("telegram rejected the message: %s", trim(out.Description))
		}
	}
	return classify(resp.StatusCode, string(respBody), nil)
}
