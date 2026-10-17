package ftp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	jftp "github.com/jlaffaye/ftp"
	"golang.org/x/net/proxy"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

type tlsMode int

const (
	tlsNone     tlsMode = iota // ftp://
	tlsImplicit                // ftps://
	tlsExplicit                // ftpes://
)

const (
	anonymousUser = "anonymous"
	anonymousPass = "anonymous@"
	dialTimeout   = 30 * time.Second
)

// target is a parsed FTP URL.
type target struct {
	host, addr string
	user, pass string
	path       string
	mode       tlsMode
}

// redacted is the URL without credentials, for logs and errors.
func (t target) redacted() string {
	scheme := "ftp"
	switch t.mode {
	case tlsImplicit:
		scheme = "ftps"
	case tlsExplicit:
		scheme = "ftpes"
	case tlsNone:
	}
	return scheme + "://" + t.addr + "/" + t.path
}

// parseTarget reads an ftp, ftps or ftpes URL.
func parseTarget(raw string) (target, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return target{}, domain.InvalidArgument("ftp: malformed URL")
	}
	t := target{host: u.Hostname()}
	port := "21"
	switch strings.ToLower(u.Scheme) {
	case "ftp":
	case "ftps":
		t.mode, port = tlsImplicit, "990"
	case "ftpes":
		t.mode = tlsExplicit
	default:
		return target{}, domain.InvalidArgument("ftp: %q is not an ftp, ftps or ftpes URL", u.Scheme+"://")
	}
	if t.host == "" {
		return target{}, domain.InvalidArgument("ftp: the URL has no host")
	}
	if p := u.Port(); p != "" {
		port = p
	}
	t.addr = net.JoinHostPort(t.host, port)
	t.user, t.pass = anonymousUser, anonymousPass
	if u.User != nil {
		t.user = u.User.Username()
		if p, ok := u.User.Password(); ok {
			t.pass = p
		} else if t.user != anonymousUser {
			t.pass = ""
		}
	}
	// Relative to the login directory; "//x" (or "/%2Fx") is absolute.
	t.path = strings.TrimPrefix(u.Path, "/")
	if strings.ContainsAny(t.path, "\r\n") {
		return target{}, domain.InvalidArgument("ftp: the path must not contain line breaks")
	}
	return t, nil
}

// remotePath is the argument of SIZE, MDTM and RETR.
func (t target) remotePath() string { return t.path }

// open connects, secures and logs in. The caller closes the connection with
// closeConn.
func open(ctx context.Context, t target, s settings.Settings, idle time.Duration) (*conn, error) {
	var tlsConf *tls.Config
	if t.mode != tlsNone {
		tlsConf = &tls.Config{
			ServerName:         t.host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: !s.TLSVerify.OrElse(true), //nolint:gosec // the user turned certificate verification off for this task
		}
	}
	dialer, err := proxyDialer(t, s)
	if err != nil {
		return nil, err
	}
	// The library has no timeouts on the control connection and, with a
	// custom dial function, leaves TLS to it; secureDial supplies both.
	out := &conn{}
	opts := []jftp.DialOption{jftp.DialWithDialFunc(secureDial(ctx, dialer, t.mode, tlsConf, idle, func(c *idleConn) { out.ctl.Store(c) }))}
	switch {
	case tlsConf != nil && t.mode == tlsImplicit:
		opts = append(opts, jftp.DialWithTLS(tlsConf))
	case tlsConf != nil:
		opts = append(opts, jftp.DialWithExplicitTLS(tlsConf))
	}
	c, err := jftp.Dial(t.addr, opts...)
	out.ServerConn = c
	if err != nil {
		return nil, classify(fmt.Errorf("ftp: connect to %s: %w", t.redacted(), err))
	}
	if err := c.Login(t.user, t.pass); err != nil {
		closeConn(out)
		return nil, classify(fmt.Errorf("ftp: login at %s: %w", t.redacted(), err))
	}
	if err := c.Type(jftp.TransferTypeBinary); err != nil {
		closeConn(out)
		return nil, classify(fmt.Errorf("ftp: binary mode at %s: %w", t.redacted(), err))
	}
	return out, nil
}

// conn is a logged-in control connection.
type conn struct {
	*jftp.ServerConn
	ctl atomic.Pointer[idleConn]
}

// drop makes every further use of the control connection fail at once, so
// that closing after an aborted transfer does not wait for a reply the
// server will not send.
func (c *conn) drop() {
	if ctl := c.ctl.Load(); ctl != nil {
		ctl.dead.Store(true)
		_ = ctl.SetDeadline(time.Now()) // unblocks a pending read
	}
}

// closeConn ends the session; the connection is gone either way.
func closeConn(c *conn) {
	if c != nil && c.ServerConn != nil {
		_ = c.Quit() // best effort: the server may have dropped us already
	}
}

// proxyDialer returns the dialer for the task's proxy, or a direct dialer.
func proxyDialer(t target, s settings.Settings) (proxy.ContextDialer, error) {
	p := s.ProxyFor(t.host)
	if p.Mode != settings.ProxyCustom {
		return &net.Dialer{Timeout: dialTimeout}, nil
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Hostname() == "" {
		return nil, fatalf("ftp: the proxy URL is malformed")
	}
	switch u.Scheme {
	case "socks5", "socks5h":
	default:
		return nil, fatalf("ftp: a %s proxy cannot carry FTP; use a socks5 proxy or disable the proxy for this host", u.Scheme)
	}
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "1080")
	}
	d, err := proxy.SOCKS5("tcp", host, auth, &net.Dialer{Timeout: dialTimeout})
	if err != nil {
		return nil, fatalf("ftp: socks5 proxy: %v", err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("ftp: socks5 dialer lacks context support")
	}
	return cd, nil
}

// secureDial dials through d and applies TLS the way the library would have:
// implicit TLS secures every connection, explicit TLS every data connection
// (the control connection is upgraded by the library after AUTH TLS). The
// first dial is the control connection; its reads and writes get an idle
// timeout so a server that never answers cannot hang the download. Data
// connections are timed by the transfer loop.
func secureDial(ctx context.Context, d proxy.ContextDialer, mode tlsMode, conf *tls.Config, idle time.Duration, onControl func(*idleConn)) func(network, address string) (net.Conn, error) {
	first := true
	return func(network, address string) (net.Conn, error) {
		control := first
		first = false
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		conn, err := d.DialContext(dctx, network, address)
		if err != nil {
			return nil, err //nolint:wrapcheck // classified by the caller
		}
		if control {
			ic := &idleConn{Conn: conn, idle: idle}
			onControl(ic)
			conn = ic
		}
		if mode == tlsImplicit || (mode == tlsExplicit && !control) {
			return tls.Client(conn, conf), nil
		}
		return conn, nil
	}
}

// idleConn gives every read and write its own deadline.
type idleConn struct {
	net.Conn
	idle time.Duration
	dead atomic.Bool
}

func (c *idleConn) Read(p []byte) (int, error) {
	if c.dead.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	_ = c.SetReadDeadline(time.Now().Add(c.idle)) // a dead connection fails the read below
	return c.Conn.Read(p)                         //nolint:wrapcheck // io.Reader contract
}

func (c *idleConn) Write(p []byte) (int, error) {
	if c.dead.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	_ = c.SetWriteDeadline(time.Now().Add(c.idle)) // a dead connection fails the write below
	return c.Conn.Write(p)                         //nolint:wrapcheck // io.Writer contract
}
