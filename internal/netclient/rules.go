package netclient

import (
	"net/http"

	"github.com/sabadia/dlm-server/internal/settings"
)

// ruleTransport routes each request through the cached transport for the
// proxy selected by the settings' per-host proxy rules. Routing happens per
// request, so redirects to another host are re-matched, and connection
// pools stay shared because transports come from the factory cache.
type ruleTransport struct {
	factory   *Factory
	settings  settings.Settings
	tlsVerify bool
}

func (t *ruleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cfg := TransportConfig{
		Proxy:     t.settings.ProxyFor(req.URL.Hostname()),
		TLSVerify: t.tlsVerify,
	}
	rt, err := t.factory.transport(cfg)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrippers must close the body on error
		}
		return nil, err
	}
	return rt.RoundTrip(req) //nolint:wrapcheck // transport errors pass through unchanged
}

// CloseIdleConnections is a no-op: the factory owns the transports.
func (*ruleTransport) CloseIdleConnections() {}
