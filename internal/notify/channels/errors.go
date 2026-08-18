package channels

import "errors"

// errNoServer reports a bare topic name with no default server configured.
// Retrying cannot fix a configuration mistake, so this is not retryable.
var errNoServer = errors.New("destination is a bare topic but no default_server is configured")

// errNoBotToken reports a Telegram channel with no token. A configuration
// mistake, so not retryable.
var errNoBotToken = errors.New("telegram bot_token is not configured")

// errNoWebhookURL reports a webhook with neither a per-user destination nor a
// default URL.
var errNoWebhookURL = errors.New("no webhook URL for this user and no default configured")

// errNoSMTPHost reports an email channel with no server configured.
var errNoSMTPHost = errors.New("email smtp_host is not configured")
