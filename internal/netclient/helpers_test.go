package netclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/settings"
)

type platformProxy = platform.ProxySettings

// hostDialer routes dials for fake host names (e.g. "origin.test") to real
// listeners and refuses everything else.
func hostDialer(routes map[string]string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		target, ok := routes[host]
		if !ok {
			// Real listeners (proxies) are dialled by IP.
			if net.ParseIP(host) == nil {
				return nil, fmt.Errorf("test dialer: unknown host %q", host)
			}
			target = addr
		}
		return d.DialContext(ctx, network, target)
	}
}

// serverAddr returns host:port of an httptest server.
func serverAddr(s *httptest.Server) string { return s.Listener.Addr().String() }

// forwardProxy is an HTTP forward proxy for plain-HTTP targets. It records
// the absolute request URLs and Proxy-Authorization headers it sees and
// answers itself instead of forwarding.
type forwardProxy struct {
	*httptest.Server
	mu    sync.Mutex
	urls  []string
	auths []string
}

func newForwardProxy(t *testing.T) *forwardProxy {
	t.Helper()
	p := &forwardProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.urls = append(p.urls, r.URL.String())
		p.auths = append(p.auths, r.Header.Get("Proxy-Authorization"))
		p.mu.Unlock()
		_, _ = io.WriteString(w, "via-proxy")
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *forwardProxy) seen() (urls, auths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.urls...), append([]string(nil), p.auths...)
}

// socksServer is a minimal SOCKS5 (RFC 1928/1929) CONNECT proxy that
// connects every request to target and records the requested addresses.
type socksServer struct {
	ln         net.Listener
	target     string
	user, pass string
	wg         sync.WaitGroup
	mu         sync.Mutex
	requests   []string // "domain:host:port" or "ip:host:port"
}

func newSOCKSServer(t *testing.T, target, user, pass string) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socksServer{ln: ln, target: target, user: user, pass: pass}
	s.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Go(func() { s.serve(c) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *socksServer) addr() string { return s.ln.Addr().String() }

func (s *socksServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *socksServer) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	methods := buf[2 : 2+int(buf[1])]
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if s.user != "" {
		if !bytes.Contains(methods, []byte{2}) {
			_, _ = c.Write([]byte{5, 0xff})
			return
		}
		_, _ = c.Write([]byte{5, 2})
		if !s.auth(c) {
			return
		}
	} else {
		_, _ = c.Write([]byte{5, 0})
	}
	if _, err := io.ReadFull(c, buf[:4]); err != nil || buf[1] != 1 {
		return
	}
	var host, kind string
	switch buf[3] {
	case 1:
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return
		}
		host, kind = net.IP(buf[:4]).String(), "ip"
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return
		}
		n := int(buf[0])
		if _, err := io.ReadFull(c, buf[:n]); err != nil {
			return
		}
		host, kind = string(buf[:n]), "domain"
	case 4:
		if _, err := io.ReadFull(c, buf[:16]); err != nil {
			return
		}
		host, kind = net.IP(buf[:16]).String(), "ip"
	default:
		return
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(buf[:2])
	s.mu.Lock()
	s.requests = append(s.requests, kind+":"+net.JoinHostPort(host, strconv.Itoa(int(port))))
	s.mu.Unlock()

	up, err := net.Dial("tcp", s.target)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_ = c.SetDeadline(time.Time{})
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
	_ = c.Close()
	_ = up.Close()
	<-done
}

func (s *socksServer) auth(c net.Conn) bool {
	b := make([]byte, 256)
	if _, err := io.ReadFull(c, b[:2]); err != nil || b[0] != 1 {
		return false
	}
	user := make([]byte, b[1])
	if _, err := io.ReadFull(c, user); err != nil {
		return false
	}
	if _, err := io.ReadFull(c, b[:1]); err != nil {
		return false
	}
	pass := make([]byte, b[0])
	if _, err := io.ReadFull(c, pass); err != nil {
		return false
	}
	if string(user) != s.user || string(pass) != s.pass {
		_, _ = c.Write([]byte{1, 1})
		return false
	}
	_, _ = c.Write([]byte{1, 0})
	return true
}

// fakeResolver resolves names from a fixed table.
type fakeResolver map[string]string

func (r fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ip, ok := r[host]
	if !ok {
		return nil, errors.New("fake resolver: no such host")
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// envMap is a goroutine-safe fake environment.
type envMap struct {
	mu sync.Mutex
	m  map[string]string
}

func (e *envMap) set(m map[string]string) {
	e.mu.Lock()
	e.m = m
	e.mu.Unlock()
}

func (e *envMap) get(k string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.m[k]
}

func newDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// quietServerLog silences expected TLS handshake failures of test servers.
func quietServerLog() *log.Logger { return log.New(io.Discard, "", 0) }

func httptestRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// noEnv is a Getenv that sees an empty environment.
func noEnv(string) string { return "" }

// noOSProxy is a SystemProxy lookup reporting no OS proxy.
func noOSProxy(context.Context) (*platformProxy, error) { return &platformProxy{}, nil }

// proxySettings returns settings using the given proxy mode/URL.
func proxySettings(mode settings.ProxyMode, url string) settings.Settings {
	return settings.Settings{Proxy: settings.Some(settings.Proxy{Mode: mode, URL: url})}
}

// get performs a GET and returns the body.
func get(t *testing.T, c *http.Client, url string, hdr http.Header) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
