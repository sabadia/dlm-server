package platform

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

const scutilSample = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
    2 : intranet.example.com
  }
  ExcludeSimpleHostnames : 1
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 8080
  HTTPProxy : proxy.example.com
  HTTPSEnable : 1
  HTTPSPort : 8443
  HTTPSProxy : secure.example.com
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 1
  SOCKSPort : 1080
  SOCKSProxy : 10.0.0.5
}
`

func TestParseScutilProxy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want *ProxySettings
	}{
		{
			name: "all enabled",
			in:   scutilSample,
			want: &ProxySettings{
				HTTP:                  "http://proxy.example.com:8080",
				HTTPS:                 "http://secure.example.com:8443",
				SOCKS:                 "socks5://10.0.0.5:1080",
				Bypass:                []string{"*.local", "169.254/16", "intranet.example.com"},
				BypassSimpleHostnames: true,
			},
		},
		{
			name: "disabled entries ignored",
			in: `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
  }
  HTTPEnable : 0
  HTTPProxy : proxy.example.com
  HTTPPort : 8080
  SOCKSEnable : 0
}`,
			want: &ProxySettings{Bypass: []string{"*.local"}},
		},
		{
			name: "no port, ipv6 host",
			in: `<dictionary> {
  HTTPEnable : 1
  HTTPProxy : fd00::1
}`,
			want: &ProxySettings{HTTP: "http://[fd00::1]"},
		},
		{name: "empty", in: "", want: &ProxySettings{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, ParseScutilProxy(tt.in)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseWindowsProxy(t *testing.T) {
	tests := []struct {
		name             string
		enabled          bool
		server, override string
		want             *ProxySettings
	}{
		{
			name: "disabled", enabled: false, server: "proxy:8080", override: "<local>",
			want: &ProxySettings{},
		},
		{
			name: "single server for all", enabled: true, server: "proxy.corp:8080",
			override: "*.corp;10.*;<local>; ",
			want: &ProxySettings{
				HTTP: "http://proxy.corp:8080", HTTPS: "http://proxy.corp:8080",
				Bypass: []string{"*.corp", "10.*"}, BypassSimpleHostnames: true,
			},
		},
		{
			name: "per protocol", enabled: true,
			server: "http=web:3128;https=https://sec:3129;ftp=ftp:21;socks=sock:1080",
			want:   &ProxySettings{HTTP: "http://web:3128", HTTPS: "https://sec:3129", SOCKS: "socks5://sock:1080"},
		},
		{name: "enabled without server", enabled: true, want: &ProxySettings{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseWindowsProxy(tt.enabled, tt.server, tt.override)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestProxySettingsNoProxy(t *testing.T) {
	p := &ProxySettings{Bypass: []string{
		"*.local", "169.254/16", "10.*", "192.168.1.*", "Example.COM", ".svc",
		"10.0.0.0/8", "fd00::/8", "1.2.3.4:8080", "*mid*", "300.*", "", "*",
	}}
	// "*.local" becomes ".local" (subdomains only); unsupported wildcards
	// and invalid octets are dropped.
	want := ".local,169.254.0.0/16,10.0.0.0/8,192.168.1.0/24,example.com,.svc,10.0.0.0/8,fd00::/8,1.2.3.4:8080,*"
	if got := p.NoProxy(); got != want {
		t.Errorf("NoProxy() =\n %q\nwant\n %q", got, want)
	}
	if (*ProxySettings)(nil).NoProxy() != "" || !(*ProxySettings)(nil).IsZero() {
		t.Error("nil settings must be empty")
	}
	if !(&ProxySettings{Bypass: []string{"x"}}).IsZero() {
		t.Error("bypass-only settings configure no proxy")
	}
	if (&ProxySettings{SOCKS: "socks5://h:1"}).IsZero() {
		t.Error("SOCKS-only settings are a proxy")
	}
}
