package ed2k

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/monkeyWie/goed2k/protocol"

	"github.com/sabadia/dlm-server/internal/domain"
)

const linkPrefix = "ed2k://|file|"

// link is a parsed ed2k file link.
type link struct {
	name    string
	size    int64
	hash    protocol.Hash
	sources []protocol.Endpoint
}

// match reports whether raw is an ed2k file link.
func match(raw string) bool {
	raw = strings.TrimSpace(raw)
	return len(raw) >= len(linkPrefix) && strings.EqualFold(raw[:len(linkPrefix)], linkPrefix)
}

// parseLink reads ed2k://|file|<name>|<size>|<hash>|[options|]/ followed by an
// optional |sources,ip:port,…|/ part. Errors match domain.ErrInvalidArgument.
func parseLink(raw string) (*link, error) {
	raw = strings.TrimSpace(raw)
	if !match(raw) {
		return nil, domain.InvalidArgument("ed2k: not an ed2k file link")
	}
	parts := strings.Split(raw[len(linkPrefix):], "|")
	// parts: name, size, hash, then options up to "/", then more segments.
	if len(parts) < 4 {
		return nil, domain.InvalidArgument("ed2k: the link must be ed2k://|file|name|size|hash|/")
	}
	name, err := url.PathUnescape(parts[0])
	if err != nil {
		return nil, domain.InvalidArgument("ed2k: the file name is not valid percent-encoding")
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || size <= 0 {
		return nil, domain.InvalidArgument("ed2k: the file size %q is not a positive number", parts[1])
	}
	hash, err := protocol.HashFromString(strings.ToUpper(parts[2]))
	if err != nil {
		return nil, domain.InvalidArgument("ed2k: the file hash must be 32 hexadecimal digits")
	}
	l := &link{name: name, size: size, hash: hash}
	sawEnd := false
	for _, p := range parts[3:] {
		switch {
		case p == "/":
			sawEnd = true
		case strings.HasPrefix(p, "sources,"):
			for _, s := range strings.Split(strings.TrimPrefix(p, "sources,"), ",") {
				if ep, ok := endpointOf(s); ok {
					l.sources = append(l.sources, ep)
				}
			}
		}
	}
	if !sawEnd {
		return nil, domain.InvalidArgument("ed2k: the link must end with |/")
	}
	return l, nil
}

// endpointOf reads an IPv4 host:port.
func endpointOf(s string) (protocol.Endpoint, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return protocol.Endpoint{}, false
	}
	p, err := strconv.Atoi(port)
	ip := net.ParseIP(host)
	if err != nil || p <= 0 || p > 65535 || ip == nil || ip.To4() == nil {
		return protocol.Endpoint{}, false
	}
	ep, err := protocol.EndpointFromString(host, p)
	if err != nil {
		return protocol.Endpoint{}, false
	}
	return ep, true
}
