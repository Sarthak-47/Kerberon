// Package channels implements the concrete delivery channels.
//
// Each is one file behind notify.Channel, and the dispatcher never changes to
// accommodate a new one (spec section 8.5).
package channels

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Sarthak-47/kerberon/internal/notify"
)

// DefaultTimeout bounds a single delivery attempt. A provider that hangs must
// not hold a worker while an incident goes unpaged.
const DefaultTimeout = 10 * time.Second

// newHTTPClient returns a client suitable for talking to a notification
// provider.
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// classify turns a transport error or an HTTP status into a delivery error,
// marking it retryable or not.
//
// The distinction is not cosmetic. Retrying something that cannot succeed
// burns the window in which the incident could still have been pursued by
// another channel, so a 4xx fails immediately and lets failover happen.
func classify(status int, body string, err error) error {
	if err != nil {
		// Timeouts and connection failures are the classic transient case.
		var netErr net.Error
		if errors.As(err, &netErr) {
			return notify.Retryable(err)
		}
		return notify.Retryable(err)
	}

	switch {
	case status >= 200 && status < 300:
		return nil

	case status == http.StatusTooManyRequests,
		status == http.StatusRequestTimeout:
		// The provider is asking us to slow down, not telling us we are wrong.
		return notify.Retryable(fmt.Errorf("status %d: %s", status, trim(body)))

	case status >= 500:
		return notify.Retryable(fmt.Errorf("status %d: %s", status, trim(body)))

	default:
		// 4xx means the request itself is wrong: a bad token, a malformed
		// destination, a topic that does not exist. Retrying cannot fix it.
		return fmt.Errorf("status %d: %s", status, trim(body))
	}
}

func trim(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
