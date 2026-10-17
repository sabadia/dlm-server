package ftp

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A minimal FTP server for the tests: passive mode (EPSV), binary transfers,
// SIZE, MDTM, REST and optional TLS, plus switches to misbehave.

type file struct {
	data []byte
	mod  time.Time
}

type serverOpts struct {
	user, pass string // empty: anonymous only
	noSize     bool
	noMDTM     bool
	noREST     bool
	// dropAfter makes the next RETR close the data connection abruptly after
	// that many bytes (once); 0 disables.
	dropAfter int
	// stallAfter makes RETR send that many bytes and then hang until the
	// server closes; stall must be true to enable it.
	stall      bool
	stallAfter int
	implicit   bool // implicit TLS
	explicit   bool // AUTH TLS
}

type fakeServer struct {
	t    *testing.T
	ln   net.Listener
	conf *tls.Config

	mu    sync.Mutex
	opts  serverOpts
	files map[string]*file
	cmds  []string
	sent  int // data bytes sent by RETR
	done  chan struct{}
	wg    sync.WaitGroup
}

func newServer(t *testing.T, opts serverOpts) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, ln: ln, opts: opts, files: map[string]*file{}, done: make(chan struct{})}
	if opts.implicit || opts.explicit {
		s.conf = testTLS(t)
	}
	if opts.implicit {
		s.ln = tls.NewListener(ln, s.conf)
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() {
		close(s.done)
		_ = s.ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) put(path string, data []byte, mod time.Time) {
	s.mu.Lock()
	s.files[path] = &file{data: data, mod: mod.UTC().Truncate(time.Second)}
	s.mu.Unlock()
}

func (s *fakeServer) set(f func(*serverOpts)) {
	s.mu.Lock()
	f(&s.opts)
	s.mu.Unlock()
}

func (s *fakeServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *fakeServer) bytesSent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func (s *fakeServer) resetCounters() {
	s.mu.Lock()
	s.cmds, s.sent = nil, 0
	s.mu.Unlock()
}

func (s *fakeServer) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(c)
		}()
	}
}

func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ftp.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
}

type session struct {
	s        *fakeServer
	conn     net.Conn
	r        *bufio.Reader
	user     string
	authed   bool
	offset   int64
	dataLn   net.Listener
	protData bool
}

func (c *session) reply(format string, args ...any) {
	_, _ = fmt.Fprintf(c.conn, format+"\r\n", args...)
}

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // test server
	c := &session{s: s, conn: conn, r: bufio.NewReader(conn)}
	defer func() {
		if c.dataLn != nil {
			_ = c.dataLn.Close()
		}
	}()
	c.reply("220 fake ftp")
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		s.mu.Lock()
		if verb == "PASS" {
			s.cmds = append(s.cmds, "PASS")
		} else {
			s.cmds = append(s.cmds, line)
		}
		o := s.opts
		s.mu.Unlock()
		if !c.handle(verb, arg, o) {
			return
		}
	}
}

func (c *session) handle(verb, arg string, o serverOpts) bool {
	s := c.s
	switch verb {
	case "AUTH":
		if !o.explicit {
			c.reply("502 no TLS")
			return true
		}
		c.reply("234 go ahead")
		tc := tls.Server(c.conn, s.conf)
		c.conn, c.r = tc, bufio.NewReader(tc)
	case "USER":
		c.user = arg
		c.reply("331 password please")
	case "PASS":
		ok := false
		if o.user == "" {
			ok = strings.EqualFold(c.user, "anonymous")
		} else {
			ok = c.user == o.user && arg == o.pass
		}
		if !ok {
			c.reply("530 login incorrect")
			return true
		}
		c.authed = true
		c.reply("230 welcome")
	case "FEAT":
		c.reply("211-Features:")
		if !o.noMDTM {
			c.reply(" MDTM")
		}
		if !o.noREST {
			c.reply(" REST STREAM")
		}
		if !o.noSize {
			c.reply(" SIZE")
		}
		c.reply("211 End")
	case "OPTS", "TYPE", "PBSZ", "NOOP":
		c.reply("200 ok")
	case "PROT":
		c.protData = arg == "P"
		c.reply("200 ok")
	case "SIZE":
		f := s.lookup(arg)
		switch {
		case o.noSize:
			c.reply("502 not implemented")
		case f == nil:
			c.reply("550 no such file")
		default:
			c.reply("213 %d", len(f.data))
		}
	case "MDTM":
		f := s.lookup(arg)
		switch {
		case o.noMDTM:
			c.reply("502 not implemented")
		case f == nil:
			c.reply("550 no such file")
		default:
			c.reply("213 %s", f.mod.Format("20060102150405"))
		}
	case "EPSV":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			c.reply("425 cannot open")
			return true
		}
		if c.dataLn != nil {
			_ = c.dataLn.Close()
		}
		c.dataLn = ln
		c.reply("229 Entering Extended Passive Mode (|||%d|)", ln.Addr().(*net.TCPAddr).Port) //nolint:errcheck,forcetypeassert // a TCP listener
	case "REST":
		if o.noREST {
			c.reply("502 REST not implemented")
			return true
		}
		n, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || n < 0 {
			c.reply("501 bad offset")
			return true
		}
		c.offset = n
		c.reply("350 restarting at %d", n)
	case "RETR":
		c.retr(arg, o)
	case "QUIT":
		c.reply("221 bye")
		return false
	default:
		c.reply("502 %s not implemented", verb)
	}
	return true
}

func (s *fakeServer) lookup(path string) *file {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[path]
}

func (c *session) retr(path string, o serverOpts) {
	s := c.s
	f := s.lookup(path)
	offset := c.offset
	c.offset = 0
	if f == nil {
		c.reply("550 no such file")
		return
	}
	if c.dataLn == nil {
		c.reply("425 use EPSV first")
		return
	}
	c.reply("150 opening data connection")
	_ = c.dataLn.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck,forcetypeassert // a TCP listener
	dc, err := c.dataLn.Accept()
	_ = c.dataLn.Close()
	c.dataLn = nil
	if err != nil {
		c.reply("425 no data connection")
		return
	}
	if o.implicit || (o.explicit && c.protData) {
		dc = tls.Server(dc, s.conf)
	}
	data := f.data
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	data = data[offset:]
	drop := false
	s.mu.Lock()
	if s.opts.dropAfter > 0 {
		drop = true
		if s.opts.dropAfter < len(data) {
			data = data[:s.opts.dropAfter]
		}
		s.opts.dropAfter = 0
	}
	stall := -1
	if s.opts.stall {
		stall = s.opts.stallAfter
	}
	s.mu.Unlock()
	if stall >= 0 && stall < len(data) {
		n, _ := dc.Write(data[:stall])
		s.addSent(n)
		<-s.done // hang until the test ends
		_ = dc.Close()
		return
	}
	n, _ := writeAll(dc, data)
	s.addSent(n)
	if drop {
		if tc, ok := dc.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // reset instead of a clean end
		}
		_ = dc.Close()
		c.reply("426 connection closed; transfer aborted")
		return
	}
	_ = dc.Close()
	c.reply("226 transfer complete")
}

func (s *fakeServer) addSent(n int) {
	s.mu.Lock()
	s.sent += n
	s.mu.Unlock()
}

func writeAll(w io.Writer, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := w.Write(b[total:])
		total += n
		if err != nil {
			return total, err //nolint:wrapcheck // test server
		}
	}
	return total, nil
}
