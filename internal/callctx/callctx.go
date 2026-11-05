// Package callctx carries who a network request is made for through a
// context: the task, and whether it came from a browser extension. Both are
// needed far below the code that knows them (a cookie lookup, a dial), where
// no task or token is in sight.
package callctx

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrLocalAddress is returned when work that a browser extension started tries
// to connect to the machine the daemon runs on. It lives here so the engines
// can recognise it (it is never worth a retry) without importing the network
// layer.
var ErrLocalAddress = errors.New("this download may not connect to the machine of the daemon")

// Caller says who a request is for.
type Caller struct {
	// TaskID is the task whose download this is; empty before the task exists
	// or for requests that belong to none.
	TaskID string
	// Restricted is set for work that an extension token started. Its
	// connections to the machine the daemon runs on are refused, and the
	// task's own cookies are sent only to it.
	Restricted bool
}

type callerKey struct{}

// With returns ctx carrying c. It adds to what ctx already carries: a request
// that is restricted stays restricted, and an empty TaskID keeps the task
// already named.
func With(ctx context.Context, c Caller) context.Context {
	prev, _ := ctx.Value(callerKey{}).(Caller)
	if c.TaskID == "" {
		c.TaskID = prev.TaskID
	}
	c.Restricted = c.Restricted || prev.Restricted
	return context.WithValue(ctx, callerKey{}, c)
}

// From returns the caller ctx carries, or the zero Caller.
func From(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// Route records how one request reaches its server. The transport that sends
// the request creates it; the proxy chooser and the dialer, which run later
// and only see the request's context, read and write it.
type Route struct{ proxied atomic.Bool }

type routeKey struct{}

// WithRoute returns ctx carrying a new Route, and the Route.
func WithRoute(ctx context.Context) (context.Context, *Route) {
	r := &Route{}
	return context.WithValue(ctx, routeKey{}, r), r
}

// MarkProxied notes that the request goes through a proxy: the connection
// being dialled is to the proxy, not to the server the request names.
func MarkProxied(ctx context.Context) {
	if r, ok := ctx.Value(routeKey{}).(*Route); ok {
		r.proxied.Store(true)
	}
}

// Proxied reports whether the request goes through a proxy.
func Proxied(ctx context.Context) bool {
	r, ok := ctx.Value(routeKey{}).(*Route)
	return ok && r.proxied.Load()
}
