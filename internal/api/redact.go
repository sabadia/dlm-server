package api

import (
	"context"
	"net/url"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Only an admin token sees what exposes a secret: the headers and body a
// browser captured (Authorization, form fields) and the credentials in a proxy
// URL. Read and extension tokens get the same objects without them.

// seesSecrets reports whether the caller may see secrets.
func seesSecrets(ctx context.Context) bool {
	p, ok := PrincipalOf(ctx)
	return !ok || p.Scope == auth.ScopeAdmin
}

// redactTask returns t without its captured headers and body; t is not
// changed. It returns t itself when there is nothing to hide.
func redactTask(t *domain.Task) *domain.Task {
	if t == nil || (len(t.Source.Headers) == 0 && len(t.Source.Body) == 0) {
		return t
	}
	c := *t
	c.Source.Headers, c.Source.Body = nil, nil
	return &c
}

// redactTaskFor redacts t for the caller of ctx.
func redactTaskFor(ctx context.Context, t *domain.Task) *domain.Task {
	if seesSecrets(ctx) {
		return t
	}
	return redactTask(t)
}

func redactTasksFor(ctx context.Context, ts []*domain.Task) []*domain.Task {
	if seesSecrets(ctx) {
		return ts
	}
	out := make([]*domain.Task, len(ts))
	for i, t := range ts {
		out[i] = redactTask(t)
	}
	return out
}

// redactEventData redacts the task carried by an event.
func redactEventData(data any) any {
	switch d := data.(type) {
	case *domain.Task:
		return redactTask(d)
	case domain.Task:
		return redactTask(&d)
	}
	return data
}

// maskProxy hides the user name and password of a proxy URL.
func maskProxy(p settings.Proxy) settings.Proxy {
	u, err := url.Parse(p.URL)
	if err != nil || u.User == nil {
		return p
	}
	u.User = url.User("***")
	p.URL = u.String()
	return p
}

// redactSettings returns s without secrets for a caller that may not see them.
func redactSettings(ctx context.Context, s settings.Settings) settings.Settings {
	if seesSecrets(ctx) {
		return s
	}
	if p, ok := s.Proxy.Get(); ok {
		s.Proxy = settings.Some(maskProxy(p))
	}
	if rules, ok := s.ProxyRules.Get(); ok {
		masked := make([]settings.ProxyRule, len(rules))
		for i, r := range rules {
			masked[i] = settings.ProxyRule{Hosts: r.Hosts, Proxy: maskProxy(r.Proxy)}
		}
		s.ProxyRules = settings.Some(masked)
	}
	s.Headers = settings.Optional[map[string]string]{}
	return s
}
