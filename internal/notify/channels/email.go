package channels

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
)

// Email delivers over SMTP.
//
// It is the universal fallback: everyone has an address, and it works when
// every push provider is having a bad day. It is also the quietest channel,
// which is why it belongs late in an escalation policy rather than first.
type Email struct {
	host     string
	port     int
	from     string
	username string
	password string
	timeout  time.Duration

	// sendMail is swapped in tests. The real one talks to a server.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// EmailConfig is what the channel needs from configuration.
type EmailConfig struct {
	Host     string
	Port     int
	From     string
	Username string
	Password string
	Timeout  time.Duration
}

// NewEmail builds the channel.
func NewEmail(c EmailConfig) *Email {
	if c.Port == 0 {
		c.Port = 587
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	e := &Email{
		host: c.Host, port: c.Port, from: c.From,
		username: c.Username, password: c.Password, timeout: c.Timeout,
	}
	e.sendMail = e.deliver
	return e
}

func (e *Email) Name() core.Channel { return core.ChannelEmail }

func (e *Email) Capabilities() notify.Capabilities {
	// An email waits until morning. Treating it as loud would let a policy
	// look louder than it is.
	return notify.Capabilities{}
}

func (e *Email) Send(ctx context.Context, m notify.Message) error {
	if e.host == "" {
		return errNoSMTPHost
	}
	to := strings.TrimSpace(m.Destination)
	if to == "" {
		return fmt.Errorf("no email address for this user")
	}

	subject := m.Title
	if subject == "" {
		subject = fmt.Sprintf("Kerberon incident %d", m.IncidentID)
	}
	if m.Severity != "" {
		subject = fmt.Sprintf("[%s] %s", strings.ToUpper(string(m.Severity)), subject)
	}

	body := m.Body
	if m.AckLink != "" && !strings.Contains(body, m.AckLink) {
		body += "\r\n\r\nAcknowledge: " + m.AckLink
	}

	// Encode the subject so a non-ASCII title survives.
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		e.from, to, mime.QEncoding.Encode("utf-8", subject), body)

	var auth smtp.Auth
	if e.username != "" {
		auth = smtp.PlainAuth("", e.username, e.password, e.host)
	}

	addr := net.JoinHostPort(e.host, fmt.Sprint(e.port))

	// smtp has no context support, so the deadline is enforced by racing the
	// send against ctx. The goroutine finishes on its own either way.
	done := make(chan error, 1)
	go func() { done <- e.sendMail(addr, auth, e.from, []string{to}, []byte(msg)) }()

	select {
	case err := <-done:
		if err != nil {
			// Most SMTP failures are transient: a greylist, a busy server, a
			// dropped connection. A permanent rejection is a 5xx reply, which
			// the message text carries.
			if isPermanentSMTP(err) {
				return err
			}
			return notify.Retryable(err)
		}
		return nil
	case <-ctx.Done():
		return notify.Retryable(ctx.Err())
	}
}

// deliver is the real SMTP conversation, with STARTTLS where offered.
func (e *Email) deliver(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, e.timeout)
	if err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, e.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: e.host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// isPermanentSMTP reports whether a server reply was a hard rejection.
//
// SMTP encodes this in the leading digit: 5xx is permanent, 4xx is "try
// again". Retrying a 5xx would burn the escalation window for nothing.
func isPermanentSMTP(err error) bool {
	msg := err.Error()
	for i := 0; i+2 < len(msg); i++ {
		if msg[i] == '5' && isDigit(msg[i+1]) && isDigit(msg[i+2]) {
			// Guard against matching a number inside prose.
			if i == 0 || msg[i-1] == ' ' || msg[i-1] == '(' {
				return true
			}
		}
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
