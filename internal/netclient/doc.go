// Package netclient builds the HTTP clients engines use (engine.ClientFactory).
//
// # Transport reuse
//
// Transports are cached by their transport-relevant settings (proxy mode and
// URL, TLS verification), so retries and the many segments of one download
// share connection pools instead of building a new transport per attempt.
// The cache is a small LRU; evicted transports have their idle connections
// closed. Each Client call returns a new, cheap *http.Client value wrapping
// a cached transport.
//
// # Timeouts
//
// Clients have no overall Timeout (downloads may run for hours). Dial, TLS
// handshake and response-header timeouts bound connection setup; detecting a
// stalled response body is the downloader's job.
//
// # Proxies
//
//   - none: direct connections.
//   - custom: http://, https:// and socks5h:// proxies use net/http's native
//     support (socks5h resolves the target host name on the proxy).
//     net/http treats socks5:// exactly like socks5h://, so socks5:// is
//     implemented here with golang.org/x/net/proxy and resolves the target
//     host name locally before connecting through the proxy (curl
//     semantics). Credentials come from the URL userinfo and are never
//     logged.
//   - system: the HTTP_PROXY, HTTPS_PROXY and NO_PROXY environment variables
//     (upper or lower case) when HTTP_PROXY or HTTPS_PROXY is set; otherwise
//     the OS manual proxy settings from platform.SystemProxy (macOS:
//     `scutil --proxy`; Windows: the WinINET registry values; Linux/BSD:
//     none, i.e. environment only, GNOME/KDE settings are not read). Both
//     sources are re-read at most every Options.SystemProxyTTL, unlike
//     http.ProxyFromEnvironment which caches the environment for the life of
//     the process. A SOCKS proxy from the OS is used when no protocol
//     specific proxy is set. In system mode every socks5:// proxy (OS or
//     environment) uses net/http's native support, i.e. remote DNS. While
//     the cached configuration is refreshed, requests wait for the lookup
//     (bounded to a few seconds). PAC / WPAD auto-configuration
//     is not supported. Like Go, loopback targets are never proxied.
//
// Per-site proxy rules are not part of the settings model yet.
//
// # Headers
//
// The settings UserAgent and Headers are applied by a RoundTripper wrapper
// only when the request does not already carry that header; the precedence
// is: request header (set by the engine or probe, including cookies added
// from the cookie jar) > settings Headers entry > settings UserAgent. Host,
// Content-Length and hop-by-hop headers in the settings are ignored.
// Sensitive settings headers (Authorization, Cookie, Proxy-Authorization...)
// are not sent after a redirect leaves the initial host and its subdomains,
// matching what net/http does for headers set on the request itself.
//
// # Redirects
//
// At most 10 redirects are followed. net/http copies the initial request's
// headers (including Range and custom headers) to every hop, keeps an
// explicit Referer, and drops Authorization, Cookie and similar headers once
// a redirect leaves the initial domain and its subdomains.
//
// # TLS
//
// TLS 1.2 is the minimum version and HTTP/2 is negotiated when the server
// supports it. TLSVerify=false disables certificate verification (logged
// once per transport). Response compression is disabled so byte ranges map
// exactly onto the file.
//
// # Extension point
//
// Options.Fallback can wrap each cached transport, e.g. with a browser TLS
// fingerprint impersonation fallback; none is built in.
package netclient
