package domain

import (
	"net/url"
	"strings"
	"time"
)

// Webhook is an HTTP endpoint that receives events. The signing secret is
// write-only: it is never part of the entity, only HasSecret says whether one
// is set.
type Webhook struct {
	ID string `json:"id"`
	// URL is the http or https endpoint that receives a POST per event.
	URL string `json:"url"`
	// Events are the event types delivered (such as "task.status").
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	HasSecret bool      `json:"hasSecret"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ValidateWebhookURL checks that raw is an absolute http(s) URL with a host
// and no credentials. It returns an error matching ErrInvalidArgument.
func ValidateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return InvalidArgument("webhook url is not valid: %v", err)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return InvalidArgument("webhook url must use http or https")
	}
	if u.Hostname() == "" {
		return InvalidArgument("webhook url needs a host")
	}
	if u.User != nil {
		return InvalidArgument("webhook url must not contain credentials; use the secret to sign deliveries")
	}
	return nil
}

// Validate checks the webhook's own fields; the event names are checked by
// the service, which knows the event catalogue.
func (w *Webhook) Validate() error {
	if w.ID == "" {
		return InvalidArgument("webhook: id is required")
	}
	if err := ValidateWebhookURL(w.URL); err != nil {
		return err
	}
	if len(w.Events) == 0 {
		return InvalidArgument("webhook %q: at least one event is required", w.ID)
	}
	return nil
}
