package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/webhook"
)

// WebhookEvents are the event types a webhook can subscribe to. task.progress
// is left out: it fires several times a second per task.
var WebhookEvents = []events.Type{
	events.TypeTaskCreated, events.TypeTaskUpdated, events.TypeTaskStatus, events.TypeTaskDeleted, events.TypeTaskRefresh,
	events.TypeQueueCreated, events.TypeQueueUpdated, events.TypeQueueDeleted, events.TypeQueueCompleted,
	events.TypeQueueActionCancelled, events.TypeSettingsUpdated, events.TypeQuotaExhausted, events.TypeQuotaReset,
}

// Sealer encrypts and decrypts secrets stored at rest (cookies.Cipher).
type Sealer interface {
	Seal(plaintext, aad []byte) ([]byte, error)
	Open(sealed, aad []byte) ([]byte, error)
}

// WebhookTester sends one event to one target and reports the outcome
// (webhook.Dispatcher).
type WebhookTester interface {
	Deliver(ctx context.Context, t webhook.Target, e events.Event) error
}

// WebhookService manages the webhooks. Secrets are sealed at rest and never
// returned.
type WebhookService struct {
	d       Deps
	sealer  Sealer
	tester  WebhookTester
	version atomic.Uint64
}

// NewWebhookService creates a WebhookService. tester may be nil, in which
// case Test is unavailable.
func NewWebhookService(d Deps, sealer Sealer, tester WebhookTester) *WebhookService {
	return &WebhookService{d: d.withDefaults(), sealer: sealer, tester: tester}
}

// CreateWebhookInput are the fields of a new webhook.
type CreateWebhookInput struct {
	URL    string
	Events []string
	// Secret signs the deliveries; empty means unsigned.
	Secret string
	// Disabled creates the webhook switched off.
	Disabled bool
}

// UpdateWebhookInput changes a webhook. Nil fields are left unchanged; an
// empty Secret removes it.
type UpdateWebhookInput struct {
	URL     *string
	Events  *[]string
	Secret  *string
	Enabled *bool
}

func normalizeWebhookEvents(in []string) ([]string, error) {
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || slices.Contains(out, e) {
			continue
		}
		if !slices.Contains(WebhookEvents, events.Type(e)) {
			return nil, domain.InvalidArgument("unknown webhook event %q", e)
		}
		out = append(out, e)
	}
	return out, nil
}

func webhookEntity(r *storage.WebhookRecord) *domain.Webhook {
	return &domain.Webhook{
		ID: r.ID, URL: r.URL, Events: r.Events, Enabled: r.Enabled, HasSecret: len(r.SealedSecret) > 0,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s *WebhookService) seal(id, secret string) ([]byte, error) {
	if secret == "" {
		return nil, nil
	}
	sealed, err := s.sealer.Seal([]byte(secret), []byte(id))
	if err != nil {
		return nil, fmt.Errorf("service: seal webhook secret: %w", err)
	}
	return sealed, nil
}

// List returns every webhook, oldest first.
func (s *WebhookService) List(ctx context.Context) ([]*domain.Webhook, error) {
	rs, err := s.d.Store.Webhooks().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list webhooks: %w", err)
	}
	out := make([]*domain.Webhook, 0, len(rs))
	for _, r := range rs {
		out = append(out, webhookEntity(r))
	}
	return out, nil
}

// Get returns one webhook.
func (s *WebhookService) Get(ctx context.Context, id string) (*domain.Webhook, error) {
	r, err := s.d.Store.Webhooks().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: get webhook: %w", err)
	}
	return webhookEntity(r), nil
}

// Create adds a webhook.
func (s *WebhookService) Create(ctx context.Context, in CreateWebhookInput) (*domain.Webhook, error) {
	evs, err := normalizeWebhookEvents(in.Events)
	if err != nil {
		return nil, fmt.Errorf("service: create webhook: %w", err)
	}
	now := s.d.now()
	rec := &storage.WebhookRecord{
		ID: s.d.NewID(), URL: strings.TrimSpace(in.URL), Events: evs, Enabled: !in.Disabled, CreatedAt: now, UpdatedAt: now,
	}
	w := webhookEntity(rec)
	if err := w.Validate(); err != nil {
		return nil, fmt.Errorf("service: create webhook: %w", err)
	}
	if rec.SealedSecret, err = s.seal(rec.ID, in.Secret); err != nil {
		return nil, err
	}
	if err := s.d.Store.Webhooks().Create(ctx, rec); err != nil {
		return nil, fmt.Errorf("service: create webhook: %w", err)
	}
	s.version.Add(1)
	publishResource(s.d.Bus, events.ResourceWebhook, rec.ID, events.OpCreated)
	return webhookEntity(rec), nil
}

// Update changes a webhook.
func (s *WebhookService) Update(ctx context.Context, id string, in UpdateWebhookInput) (*domain.Webhook, error) {
	var rec *storage.WebhookRecord
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		if rec, err = tx.Webhooks().Get(ctx, id); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if in.URL != nil {
			rec.URL = strings.TrimSpace(*in.URL)
		}
		if in.Events != nil {
			if rec.Events, err = normalizeWebhookEvents(*in.Events); err != nil {
				return err
			}
		}
		if in.Enabled != nil {
			rec.Enabled = *in.Enabled
		}
		if in.Secret != nil {
			if rec.SealedSecret, err = s.seal(id, *in.Secret); err != nil {
				return err
			}
		}
		if err := webhookEntity(rec).Validate(); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		rec.UpdatedAt = s.d.now()
		return tx.Webhooks().Update(ctx, rec) //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return nil, fmt.Errorf("service: update webhook: %w", err)
	}
	s.version.Add(1)
	publishResource(s.d.Bus, events.ResourceWebhook, id, events.OpUpdated)
	return webhookEntity(rec), nil
}

// Delete removes a webhook.
func (s *WebhookService) Delete(ctx context.Context, id string) error {
	if err := s.d.Store.Webhooks().Delete(ctx, id); err != nil {
		return fmt.Errorf("service: delete webhook: %w", err)
	}
	s.version.Add(1)
	publishResource(s.d.Bus, events.ResourceWebhook, id, events.OpDeleted)
	return nil
}

// ErrWebhookTestUnavailable means the daemon has no dispatcher to test with.
var ErrWebhookTestUnavailable = errors.New("service: webhook testing is not available")

// Test sends a "webhook.test" event to the webhook now (even when it is
// disabled or not subscribed to it) and returns the delivery error, if any.
func (s *WebhookService) Test(ctx context.Context, id string) error {
	if s.tester == nil {
		return ErrWebhookTestUnavailable
	}
	rec, err := s.d.Store.Webhooks().Get(ctx, id)
	if err != nil {
		return fmt.Errorf("service: test webhook: %w", err)
	}
	t, err := s.target(rec)
	if err != nil {
		return err
	}
	ev := events.Event{Type: "webhook.test", Time: s.d.now(), Data: map[string]string{"webhookId": id}}
	if err := s.tester.Deliver(ctx, t, ev); err != nil {
		return fmt.Errorf("service: test webhook: %w", err)
	}
	return nil
}

func (s *WebhookService) target(r *storage.WebhookRecord) (webhook.Target, error) {
	t := webhook.Target{ID: r.ID, URL: r.URL, Events: r.Events}
	if len(r.SealedSecret) > 0 {
		secret, err := s.sealer.Open(r.SealedSecret, []byte(r.ID))
		if err != nil {
			return t, fmt.Errorf("service: open webhook %q secret: %w", r.ID, err)
		}
		t.Secret = secret
	}
	return t, nil
}

// Targets returns the enabled webhooks for the dispatcher. A webhook whose
// secret cannot be opened is skipped rather than sent unsigned.
func (s *WebhookService) Targets(ctx context.Context) ([]webhook.Target, error) {
	rs, err := s.d.Store.Webhooks().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list webhooks: %w", err)
	}
	var out []webhook.Target
	for _, r := range rs {
		if !r.Enabled {
			continue
		}
		t, err := s.target(r)
		if err != nil {
			s.d.Logger.WarnContext(ctx, "skipping webhook", "webhook", r.ID, "error", err)
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// Version changes whenever a webhook is created, updated or deleted.
func (s *WebhookService) Version() uint64 { return s.version.Load() }

// Deliveries returns the recent delivery attempts to a webhook, newest first
// (the dispatcher keeps the last webhook.DeliveryLogSize per webhook in memory).
// An unknown webhook matches domain.ErrNotFound.
func (s *WebhookService) Deliveries(ctx context.Context, id string) ([]webhook.Delivery, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	log, ok := s.tester.(interface {
		Deliveries(webhookID string) []webhook.Delivery
	})
	if !ok {
		return []webhook.Delivery{}, nil
	}
	return log.Deliveries(id), nil
}
