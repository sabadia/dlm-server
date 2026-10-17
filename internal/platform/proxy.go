package platform

import (
	"bufio"
	"net"
	"strconv"
	"strings"
)

// ProxySettings is the operating system's manual proxy configuration.
//
// Proxy fields are URLs with a scheme ("http://host:port",
// "socks5://host:port"); an empty field means "no proxy for that protocol".
// PAC files / auto-configuration (WPAD) are not supported and are ignored.
type ProxySettings struct {
	// HTTP is the proxy for plain-HTTP requests.
	HTTP string
	// HTTPS is the proxy for HTTPS requests (an HTTP CONNECT proxy).
	HTTPS string
	// SOCKS is a SOCKS proxy, used for protocols without a dedicated proxy.
	SOCKS string
	// Bypass lists hosts that must be reached directly, in the OS's own
	// syntax (e.g. "*.local", "10.*", "169.254/16", "example.com").
	Bypass []string
	// BypassSimpleHostnames bypasses the proxy for host names without a dot
	// (Windows "<local>", macOS "ExcludeSimpleHostnames").
	BypassSimpleHostnames bool
}

// IsZero reports whether no proxy is configured.
func (p *ProxySettings) IsZero() bool {
	return p == nil || (p.HTTP == "" && p.HTTPS == "" && p.SOCKS == "")
}

// NoProxy converts Bypass into the NO_PROXY syntax understood by
// golang.org/x/net/http/httpproxy (comma separated domains, IPs and CIDRs).
// Patterns it cannot express (wildcards other than a leading "*." or a
// trailing ".*" after IPv4 octets) are dropped.
func (p *ProxySettings) NoProxy() string {
	if p == nil {
		return ""
	}
	out := make([]string, 0, len(p.Bypass))
	for _, b := range p.Bypass {
		if v, ok := noProxyEntry(b); ok {
			out = append(out, v)
		}
	}
	return strings.Join(out, ",")
}

func noProxyEntry(pattern string) (string, bool) {
	b := strings.ToLower(strings.TrimSpace(pattern))
	switch {
	case b == "":
		return "", false
	case b == "*":
		return b, true
	case strings.HasPrefix(b, "*.") && !strings.Contains(b[2:], "*"):
		return b[1:], true // "*.example.com" → ".example.com" (subdomains only)
	case strings.HasSuffix(b, ".*") && !strings.Contains(b[:len(b)-2], "*"):
		return ipv4PrefixCIDR(strings.Split(b[:len(b)-2], "."))
	case strings.Contains(b, "*"):
		return "", false
	case strings.Contains(b, "/"):
		addr, bits, _ := strings.Cut(b, "/")
		if strings.Contains(addr, ":") {
			return b, true // IPv6 CIDR
		}
		parts := strings.Split(addr, ".")
		if len(parts) < 4 && validOctets(parts) {
			for len(parts) < 4 {
				parts = append(parts, "0") // macOS shorthand "169.254/16"
			}
			return strings.Join(parts, ".") + "/" + bits, true
		}
		return b, true
	default:
		return b, true
	}
}

// ipv4PrefixCIDR turns the octets of "10.1.*" into "10.1.0.0/16".
func ipv4PrefixCIDR(parts []string) (string, bool) {
	if len(parts) == 0 || len(parts) > 3 || !validOctets(parts) {
		return "", false
	}
	bits := 8 * len(parts)
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	return strings.Join(parts, ".") + "/" + strconv.Itoa(bits), true
}

func validOctets(parts []string) bool {
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

// ParseScutilProxy parses the output of macOS `scutil --proxy`.
func ParseScutilProxy(out string) *ProxySettings {
	values := map[string]string{}
	var bypass []string
	inExceptions := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if inExceptions {
			if line == "}" {
				inExceptions = false
				continue
			}
			if _, v, ok := strings.Cut(line, " : "); ok {
				bypass = append(bypass, strings.TrimSpace(v))
			}
			continue
		}
		k, v, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "ExceptionsList" && strings.HasPrefix(v, "<array>") {
			inExceptions = true
			continue
		}
		values[k] = v
	}
	p := &ProxySettings{Bypass: bypass, BypassSimpleHostnames: values["ExcludeSimpleHostnames"] == "1"}
	entry := func(prefix, scheme string) string {
		if values[prefix+"Enable"] != "1" || values[prefix+"Proxy"] == "" {
			return ""
		}
		return proxyURL(scheme, values[prefix+"Proxy"], values[prefix+"Port"])
	}
	p.HTTP = entry("HTTP", "http")
	p.HTTPS = entry("HTTPS", "http")
	p.SOCKS = entry("SOCKS", "socks5")
	return p
}

// ParseWindowsProxy interprets the WinINET registry values ProxyEnable,
// ProxyServer and ProxyOverride from
// HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings.
//
// ProxyServer is either "host:port" (used for HTTP and HTTPS) or a list
// such as "http=host:port;https=host:port;socks=host:port". A "socks"
// entry is mapped to socks5 (Go has no SOCKS4 client). ProxyOverride is a
// ';'-separated bypass list where "<local>" means simple host names.
func ParseWindowsProxy(enabled bool, server, override string) *ProxySettings {
	p := &ProxySettings{}
	if !enabled {
		return p
	}
	server = strings.TrimSpace(server)
	if strings.Contains(server, "=") {
		for item := range strings.SplitSeq(server, ";") {
			proto, addr, ok := strings.Cut(strings.TrimSpace(item), "=")
			if !ok || strings.TrimSpace(addr) == "" {
				continue
			}
			addr = strings.TrimSpace(addr)
			switch strings.ToLower(strings.TrimSpace(proto)) {
			case "http":
				p.HTTP = withScheme(addr, "http")
			case "https":
				p.HTTPS = withScheme(addr, "http")
			case "socks":
				p.SOCKS = withScheme(addr, "socks5")
			default: // ftp and unknown protocols are irrelevant here
			}
		}
	} else if server != "" {
		p.HTTP = withScheme(server, "http")
		p.HTTPS = p.HTTP
	}
	for item := range strings.SplitSeq(override, ";") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
		case strings.EqualFold(item, "<local>"):
			p.BypassSimpleHostnames = true
		default:
			p.Bypass = append(p.Bypass, item)
		}
	}
	return p
}

func withScheme(addr, scheme string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return scheme + "://" + addr
}

func proxyURL(scheme, host, port string) string {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if port == "" || port == "0" {
		if strings.Contains(host, ":") {
			return scheme + "://[" + host + "]"
		}
		return scheme + "://" + host
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}
