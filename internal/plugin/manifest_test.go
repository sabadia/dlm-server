package plugin

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(`{"name":"my-host","version":"1.2.0","hooks":{"resolve":{"match":["https://*.example.com/*"]},"done":{}},"net":["api.example.com","*.cdn.example"],"timeout":"15s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Main != "index.js" {
		t.Errorf("main = %q, want the default", m.Main)
	}
	if got := m.HookNames(); !slices.Equal(got, []string{HookResolve, HookDone}) {
		t.Errorf("hooks = %v", got)
	}
	if d, _ := m.CallTimeout(); d != 15*time.Second {
		t.Errorf("timeout = %s", d)
	}
}

func TestManifestRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"not json":         `{`,
		"no name":          `{"version":"1","hooks":{"done":{}}}`,
		"upper case name":  `{"name":"My","version":"1","hooks":{"done":{}}}`,
		"path in name":     `{"name":"a/b","version":"1","hooks":{"done":{}}}`,
		"no version":       `{"name":"a","hooks":{"done":{}}}`,
		"no hooks":         `{"name":"a","version":"1"}`,
		"unknown hook":     `{"name":"a","version":"1","hooks":{"explode":{}}}`,
		"empty pattern":    `{"name":"a","version":"1","hooks":{"resolve":{"match":[" "]}}}`,
		"absolute main":    `{"name":"a","version":"1","main":"/etc/x.js","hooks":{"done":{}}}`,
		"escaping main":    `{"name":"a","version":"1","main":"../x.js","hooks":{"done":{}}}`,
		"not a script":     `{"name":"a","version":"1","main":"x.sh","hooks":{"done":{}}}`,
		"url as net host":  `{"name":"a","version":"1","hooks":{"done":{}},"net":["https://x.example"]}`,
		"port in net host": `{"name":"a","version":"1","hooks":{"done":{}},"net":["x.example:80"]}`,
		"bad timeout":      `{"name":"a","version":"1","hooks":{"done":{}},"timeout":"soon"}`,
		"too long timeout": `{"name":"a","version":"1","hooks":{"done":{}},"timeout":"5m"}`,
		"negative timeout": `{"name":"a","version":"1","hooks":{"done":{}},"timeout":"-1s"}`,
	} {
		if _, err := ParseManifest([]byte(in)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	many := `{"name":"a","version":"1","hooks":{"done":{}},"net":[` + strings.Repeat(`"a.example",`, maxNet) + `"b.example"]}`
	if _, err := ParseManifest([]byte(many)); err == nil {
		t.Error("too many net hosts accepted")
	}
}

func TestMatchesAndHosts(t *testing.T) {
	m, err := ParseManifest([]byte(`{"name":"a","version":"1","hooks":{"resolve":{"match":["https://*.example.com/watch?*","http://exact.test/x"]},"done":{}},"net":["api.example.com","*.cdn.example"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]bool{
		"https://www.example.com/watch?v=1": true,
		"HTTPS://WWW.EXAMPLE.COM/watch?v=1": true,
		"https://example.com/watch?v=1":     false, // "*." needs a label
		"https://www.example.com/other":     false,
		"http://exact.test/x":               true,
		"http://exact.test/x/y":             false,
	} {
		if got := m.Matches(HookResolve, url); got != want {
			t.Errorf("Matches(resolve, %q) = %v", url, got)
		}
	}
	if !m.Matches(HookDone, "anything") {
		t.Error("a hook without patterns must match every link")
	}
	if m.Matches(HookStart, "https://www.example.com/watch?v=1") {
		t.Error("an undeclared hook matched")
	}
	for host, want := range map[string]bool{
		"api.example.com": true, "API.example.com": true, "x.cdn.example": true, "a.b.cdn.example": true,
		"cdn.example": false, "evil.com": false, "api.example.com.evil.com": false, "notapi.example.com": false,
	} {
		if got := m.AllowsHost(host); got != want {
			t.Errorf("AllowsHost(%q) = %v", host, got)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"*", "", true},
		{"a*c", "abbbc", true},
		{"a*c", "ab", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "acb", false},
		{"*x", "yx", true},
		{"x*", "xy", true},
		{"*mid*", "a mid b", true},
		{"a**b", "ab", true},
	} {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}
