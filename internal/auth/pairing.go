package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Pairing limits.
const (
	// PairingTTL is how long a pairing waits for the user to allow or deny it.
	PairingTTL = 2 * time.Minute
	// PairingClaimTTL is how long the client has, after the user allowed a
	// pairing, to fetch the token it was issued. An unclaimed token is revoked.
	PairingClaimTTL = 5 * time.Minute
	// MaxPendingPairings bounds the pairings waiting for an answer, so a
	// misbehaving local program cannot bury the user in prompts.
	MaxPendingPairings = 5

	// pairingKeep is how long a finished pairing stays visible to its client.
	pairingKeep = 10 * time.Minute
	maxClientID = 128
	maxVersion  = 32
)

// PairingStatus is where a pairing is.
type PairingStatus string

// Pairing statuses.
const (
	// PairingPending waits for the user to allow or deny.
	PairingPending PairingStatus = "pending"
	// PairingApproved was allowed; the client can fetch its token once.
	PairingApproved PairingStatus = "approved"
	// PairingDenied was refused by the user.
	PairingDenied PairingStatus = "denied"
	// PairingExpired was not answered in time, or its token was not fetched in time.
	PairingExpired PairingStatus = "expired"
)

// Browsers a pairing may name.
var pairingBrowsers = []string{"chrome", "chromium", "edge", "brave", "opera", "vivaldi", "firefox", "other"}

// PairingStart is what a client says about itself when it asks to pair.
type PairingStart struct {
	// Name is shown to the user and names the token; default "<browser> extension".
	Name string
	// Browser is one of chrome, chromium, edge, brave, opera, vivaldi, firefox, other.
	Browser string
	// ClientID is the extension's id (Chrome) or add-on id (Firefox).
	ClientID string
	Version  string
}

// Pairing is a client's request to be given a token, as the user sees it.
type Pairing struct {
	ID            string        `json:"id"`
	Status        PairingStatus `json:"status"`
	Name          string        `json:"name"`
	Browser       string        `json:"browser"`
	ClientID      string        `json:"clientId,omitempty"`
	ClientVersion string        `json:"clientVersion,omitempty"`
	// Code is six digits the client shows and the user compares with the
	// prompt, so they allow the program they started and not another.
	Code      string    `json:"code"`
	CreatedAt time.Time `json:"createdAt"`
	// ExpiresAt is when a pending pairing is dropped, or when an approved
	// one's token must have been fetched.
	ExpiresAt time.Time `json:"expiresAt"`
	// TokenID is the issued token's id once the pairing is approved.
	TokenID string `json:"tokenId,omitempty"`
}

// PairingEvent tells the daemon that a pairing appeared or changed, so it can
// publish the event.
type PairingEvent struct {
	// Requested is true for a new pairing, false for one that was answered or
	// expired.
	Requested bool
	Pairing   Pairing
}

type pairing struct {
	Pairing
	secret  string // the issued token, until the client fetches it
	claimed bool
}

// Pairings holds the pairings that wait for an answer or for their token to be
// fetched. They live in memory only: a restart drops them, and a client asks
// again. It is safe for concurrent use.
type Pairings struct {
	reg    *Registry
	clock  func() time.Time
	notify func(PairingEvent)

	mu    sync.Mutex
	items map[string]*pairing
}

// NewPairings creates the manager. notify may be nil.
func NewPairings(reg *Registry, notify func(PairingEvent)) *Pairings {
	return &Pairings{reg: reg, clock: time.Now, notify: notify, items: map[string]*pairing{}}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func sixDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func validatePlain(field, v string, limit int) error {
	if len(v) > limit {
		return domain.InvalidArgumentCode("pairing.bad_field", map[string]string{"field": field}, "auth: %s has at most %d characters", field, limit)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return domain.InvalidArgumentCode("pairing.bad_field", map[string]string{"field": field}, "auth: %s must not contain control characters", field)
		}
	}
	return nil
}

// Start records a client's request to pair and tells the daemon, which shows
// it to the user. At most MaxPendingPairings may wait at once.
func (p *Pairings) Start(ctx context.Context, in PairingStart) (Pairing, error) {
	in.Browser = strings.ToLower(strings.TrimSpace(in.Browser))
	if in.Browser == "" {
		in.Browser = "other"
	}
	if !slices.Contains(pairingBrowsers, in.Browser) {
		return Pairing{}, domain.InvalidArgumentCode("pairing.bad_browser", nil, "auth: browser must be one of %s", strings.Join(pairingBrowsers, ", "))
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = strings.ToUpper(in.Browser[:1]) + in.Browser[1:] + " extension"
	}
	if err := ValidateName(in.Name); err != nil {
		return Pairing{}, err
	}
	if err := validatePlain("clientId", in.ClientID, maxClientID); err != nil {
		return Pairing{}, err
	}
	if err := validatePlain("version", in.Version, maxVersion); err != nil {
		return Pairing{}, err
	}
	id, err := randomHex(16)
	if err != nil {
		return Pairing{}, err
	}
	code, err := sixDigits()
	if err != nil {
		return Pairing{}, err
	}

	p.mu.Lock()
	now := p.clock()
	events := p.sweepLocked(ctx, now)
	pending := 0
	for _, it := range p.items {
		if it.Status == PairingPending {
			pending++
		}
	}
	if pending >= MaxPendingPairings {
		p.mu.Unlock()
		p.emit(events)
		return Pairing{}, domain.ConflictCode("pairing.too_many", nil, "auth: %d pairings already wait for an answer", pending)
	}
	it := &pairing{Pairing: Pairing{
		ID: id, Status: PairingPending, Name: in.Name, Browser: in.Browser, ClientID: in.ClientID, ClientVersion: in.Version,
		Code: code, CreatedAt: now.UTC(), ExpiresAt: now.Add(PairingTTL).UTC(),
	}}
	p.items[id] = it
	out := it.Pairing
	p.mu.Unlock()

	p.emit(append(events, PairingEvent{Requested: true, Pairing: out}))
	return out, nil
}

// Pending lists the pairings waiting for an answer, oldest first.
func (p *Pairings) Pending(ctx context.Context) []Pairing {
	p.mu.Lock()
	events := p.sweepLocked(ctx, p.clock())
	var out []Pairing
	for _, it := range p.items {
		if it.Status == PairingPending {
			out = append(out, it.Pairing)
		}
	}
	p.mu.Unlock()
	p.emit(events)
	slices.SortFunc(out, func(a, b Pairing) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Get returns a pairing. The first time it is called after the pairing was
// approved it also returns the token, once; later calls return an empty token.
// An unknown (or long finished) pairing matches domain.ErrNotFound.
func (p *Pairings) Get(ctx context.Context, id string) (Pairing, string, error) {
	p.mu.Lock()
	events := p.sweepLocked(ctx, p.clock())
	it, ok := p.items[id]
	if !ok {
		p.mu.Unlock()
		p.emit(events)
		return Pairing{}, "", domain.NotFoundCode("pairing.not_found", map[string]string{"id": id}, "pairing %q not found", id)
	}
	out, secret := it.Pairing, ""
	if it.Status == PairingApproved && !it.claimed {
		secret, it.secret, it.claimed = it.secret, "", true
	}
	p.mu.Unlock()
	p.emit(events)
	return out, secret, nil
}

// Approve lets a pending pairing through: it issues the extension token, which
// the client fetches with Get. Anything but a pending pairing matches
// domain.ErrConflict.
func (p *Pairings) Approve(ctx context.Context, id string) (Pairing, error) {
	p.mu.Lock()
	now := p.clock()
	events := p.sweepLocked(ctx, now)
	it, err := p.pendingLocked(id)
	if err != nil {
		p.mu.Unlock()
		p.emit(events)
		return Pairing{}, err
	}
	secret, tok, err := p.reg.CreateExtension(ctx, it.Name, ExtensionInfo{Browser: it.Browser, ID: it.ClientID, Version: it.ClientVersion})
	if err != nil {
		p.mu.Unlock()
		p.emit(events)
		return Pairing{}, err
	}
	it.Status, it.secret, it.TokenID, it.ExpiresAt = PairingApproved, secret, tok.ID, now.Add(PairingClaimTTL).UTC()
	out := it.Pairing
	p.mu.Unlock()
	p.emit(append(events, PairingEvent{Pairing: out}))
	return out, nil
}

// Deny refuses a pending pairing. Anything but a pending pairing matches
// domain.ErrConflict.
func (p *Pairings) Deny(ctx context.Context, id string) (Pairing, error) {
	p.mu.Lock()
	events := p.sweepLocked(ctx, p.clock())
	it, err := p.pendingLocked(id)
	if err != nil {
		p.mu.Unlock()
		p.emit(events)
		return Pairing{}, err
	}
	it.Status = PairingDenied
	out := it.Pairing
	p.mu.Unlock()
	p.emit(append(events, PairingEvent{Pairing: out}))
	return out, nil
}

// Sweep expires pairings that ran out of time and revokes approved tokens
// nobody came for. Other calls do it too, but only when somebody asks; Run
// makes sure it happens anyway.
func (p *Pairings) Sweep(ctx context.Context) {
	p.mu.Lock()
	events := p.sweepLocked(ctx, p.clock())
	p.mu.Unlock()
	p.emit(events)
}

// Run sweeps every interval until ctx ends.
func (p *Pairings) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.Sweep(ctx)
		}
	}
}

func (p *Pairings) pendingLocked(id string) (*pairing, error) {
	it, ok := p.items[id]
	if !ok {
		return nil, domain.NotFoundCode("pairing.not_found", map[string]string{"id": id}, "pairing %q not found", id)
	}
	if it.Status != PairingPending {
		return nil, domain.ConflictCode("pairing.not_pending", map[string]string{"id": id, "status": string(it.Status)}, "pairing %q is %s", id, it.Status)
	}
	return it, nil
}

// sweepLocked expires what ran out of time, revokes tokens nobody fetched and
// forgets old pairings. It returns the events to publish once the lock is
// released. The caller holds p.mu.
func (p *Pairings) sweepLocked(ctx context.Context, now time.Time) []PairingEvent {
	var events []PairingEvent
	for id, it := range p.items {
		switch {
		case it.Status == PairingPending && !now.Before(it.ExpiresAt):
			it.Status = PairingExpired
			events = append(events, PairingEvent{Pairing: it.Pairing})
		case it.Status == PairingApproved && !it.claimed && !now.Before(it.ExpiresAt):
			// The client never came for its token: nobody holds it, so revoke it.
			_ = p.reg.Delete(ctx, it.TokenID) // best effort: a token nobody knows is harmless and the user can still revoke it
			it.Status, it.secret = PairingExpired, ""
			events = append(events, PairingEvent{Pairing: it.Pairing})
		case it.Status != PairingPending && it.Status != PairingApproved && now.Sub(it.ExpiresAt) > pairingKeep,
			it.Status == PairingApproved && it.claimed && now.Sub(it.ExpiresAt) > pairingKeep:
			delete(p.items, id)
		}
	}
	return events
}

func (p *Pairings) emit(events []PairingEvent) {
	if p.notify == nil {
		return
	}
	for _, e := range events {
		p.notify(e)
	}
}
