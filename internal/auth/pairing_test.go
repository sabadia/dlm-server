package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) time() time.Time         { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newPairings(t *testing.T) (*Pairings, *Registry, *fakeClock, *[]PairingEvent) {
	t.Helper()
	reg, _ := newRegistry(t)
	clk := &fakeClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	var events []PairingEvent
	p := NewPairings(reg, func(e PairingEvent) { events = append(events, e) })
	p.clock = clk.time
	reg.clock = clk.time
	return p, reg, clk, &events
}

func TestPairingApproveIssuesAnExtensionTokenOnce(t *testing.T) {
	p, reg, _, events := newPairings(t)
	got, err := p.Start(t.Context(), PairingStart{Browser: "Chrome", ClientID: "abc", Version: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != PairingPending || got.Browser != "chrome" || got.Name != "Chrome extension" || len(got.Code) != 6 || got.ID == "" {
		t.Fatalf("pairing = %+v", got)
	}
	if len(*events) != 1 || !(*events)[0].Requested {
		t.Fatalf("events = %+v, want one request", *events)
	}
	if pending := p.Pending(t.Context()); len(pending) != 1 || pending[0].ID != got.ID {
		t.Fatalf("pending = %+v", pending)
	}

	// Nothing to fetch before the user answers.
	if st, tok, _ := p.Get(t.Context(), got.ID); st.Status != PairingPending || tok != "" {
		t.Fatalf("before approval: %+v token %q", st, tok)
	}
	approved, err := p.Approve(t.Context(), got.ID)
	if err != nil || approved.Status != PairingApproved || approved.TokenID == "" {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	if n := len(*events); n != 2 || (*events)[1].Requested || (*events)[1].Pairing.Status != PairingApproved {
		t.Fatalf("events = %+v", *events)
	}

	st, secret, err := p.Get(t.Context(), got.ID)
	if err != nil || st.Status != PairingApproved || !strings.HasPrefix(secret, TokenPrefix) {
		t.Fatalf("first read = %+v, %q, %v", st, secret, err)
	}
	if _, again, _ := p.Get(t.Context(), got.ID); again != "" {
		t.Errorf("the token was handed out twice")
	}

	pr, ok := reg.Authenticate(t.Context(), secret)
	if !ok || pr.Scope != ScopeExtension {
		t.Fatalf("principal = %+v, %v", pr, ok)
	}
	list, _ := reg.List(t.Context())
	if len(list) != 1 || list[0].Kind != KindExtension || list[0].Browser != "chrome" || list[0].ClientID != "abc" || list[0].ClientVersion != "0.1.0" {
		t.Fatalf("listing = %+v", list)
	}
}

func TestPairingDenyAndStates(t *testing.T) {
	p, _, _, events := newPairings(t)
	got, _ := p.Start(t.Context(), PairingStart{Browser: "firefox", Name: "My Firefox"})
	denied, err := p.Deny(t.Context(), got.ID)
	if err != nil || denied.Status != PairingDenied {
		t.Fatalf("deny = %+v, %v", denied, err)
	}
	if n := len(*events); n != 2 || (*events)[1].Pairing.Status != PairingDenied {
		t.Fatalf("events = %+v", *events)
	}
	for _, f := range []func() error{
		func() error { _, err := p.Approve(t.Context(), got.ID); return err },
		func() error { _, err := p.Deny(t.Context(), got.ID); return err },
	} {
		if err := f(); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("answering a denied pairing: %v, want a conflict", err)
		}
	}
	if _, err := p.Approve(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	if _, _, err := p.Get(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	if len(p.Pending(t.Context())) != 0 {
		t.Error("a denied pairing is still pending")
	}
}

func TestPairingExpiresAndRevokesAnUnclaimedToken(t *testing.T) {
	p, reg, clk, events := newPairings(t)

	// Nobody answers.
	a, _ := p.Start(t.Context(), PairingStart{Browser: "edge"})
	clk.advance(PairingTTL)
	if st, _, _ := p.Get(t.Context(), a.ID); st.Status != PairingExpired {
		t.Fatalf("status = %s, want expired", st.Status)
	}
	if _, err := p.Approve(t.Context(), a.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("approving an expired pairing: %v", err)
	}
	if last := (*events)[len(*events)-1]; last.Requested || last.Pairing.Status != PairingExpired {
		t.Errorf("last event = %+v", last)
	}

	// Allowed, but the extension never comes for the token.
	b, _ := p.Start(t.Context(), PairingStart{Browser: "brave"})
	if _, err := p.Approve(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := reg.List(t.Context()); len(list) != 1 {
		t.Fatalf("tokens = %+v", list)
	}
	clk.advance(PairingClaimTTL)
	if st, tok, _ := p.Get(t.Context(), b.ID); st.Status != PairingExpired || tok != "" {
		t.Fatalf("unclaimed: %+v token %q", st, tok)
	}
	if list, _ := reg.List(t.Context()); len(list) != 0 {
		t.Errorf("the unclaimed token was not revoked: %+v", list)
	}

	// A finished pairing is forgotten after a while.
	clk.advance(time.Hour)
	if _, _, err := p.Get(t.Context(), b.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("old pairing: %v, want not found", err)
	}
}

func TestPairingLimitsAndValidation(t *testing.T) {
	p, _, clk, _ := newPairings(t)
	for i := range MaxPendingPairings {
		if _, err := p.Start(t.Context(), PairingStart{Browser: "chrome"}); err != nil {
			t.Fatalf("pairing %d: %v", i, err)
		}
	}
	if _, err := p.Start(t.Context(), PairingStart{Browser: "chrome"}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("one too many: %v, want a conflict", err)
	}
	clk.advance(PairingTTL)
	if _, err := p.Start(t.Context(), PairingStart{Browser: "chrome"}); err != nil {
		t.Errorf("after the old ones expired: %v", err)
	}

	for name, in := range map[string]PairingStart{
		"unknown browser": {Browser: "netscape"},
		"long name":       {Browser: "chrome", Name: strings.Repeat("x", 65)},
		"control in name": {Browser: "chrome", Name: "a\x00b"},
		"long client id":  {Browser: "chrome", ClientID: strings.Repeat("x", 129)},
		"long version":    {Browser: "chrome", Version: strings.Repeat("1", 33)},
		"control version": {Browser: "chrome", Version: "1\n2"},
	} {
		if _, err := p.Start(t.Context(), in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: %v, want invalid argument", name, err)
		}
	}
	if got, err := p.Start(t.Context(), PairingStart{}); err != nil || got.Browser != "other" {
		t.Errorf("default browser: %+v, %v", got, err)
	}
}

func TestPairingSameNameGetsANumber(t *testing.T) {
	p, reg, _, _ := newPairings(t)
	for range 3 {
		got, err := p.Start(t.Context(), PairingStart{Browser: "chrome"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Approve(t.Context(), got.ID); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := reg.List(t.Context())
	var names []string
	for _, tok := range list {
		names = append(names, tok.Name)
	}
	if strings.Join(names, ",") != "Chrome extension,Chrome extension (2),Chrome extension (3)" {
		t.Errorf("names = %v", names)
	}
}

func TestSweepRevokesATokenNobodyCameFor(t *testing.T) {
	p, reg, clk, events := newPairings(t)
	started, err := p.Start(t.Context(), PairingStart{Browser: "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Approve(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	*events = nil
	clk.advance(time.Hour) // nobody fetched the token in time
	p.Sweep(t.Context())   // no request came in: the timer does it
	if len(*events) != 1 || (*events)[0].Pairing.Status != PairingExpired {
		t.Fatalf("events after the sweep = %+v", *events)
	}
	if list, err := reg.List(t.Context()); err != nil || len(list) != 0 {
		t.Errorf("tokens after the sweep = %v (%v), want none", list, err)
	}
}
