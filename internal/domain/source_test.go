package domain

import (
	"errors"
	"testing"
)

func TestSourceValidate(t *testing.T) {
	tests := []struct {
		name    string
		src     Source
		wantErr bool
	}{
		{"http", Source{CurrentURL: "https://example.com/a"}, false},
		{"magnet", Source{CurrentURL: "magnet:?xt=urn:btih:abc"}, false},
		{"ed2k link", Source{CurrentURL: "ed2k://|file|a%20b.iso|1234|0123456789ABCDEF0123456789ABCDEF|/|sources,1.2.3.4:4662|/"}, false},
		{"ed2k link upper case", Source{CurrentURL: "ED2K://|file|a|1|0123456789ABCDEF0123456789ABCDEF|/"}, false},
		{"data url", Source{CurrentURL: "data:application/x-bittorrent;base64,AAAA"}, false},
		{"ftp with all refs", Source{
			CurrentURL: "ftp://example.com/a", OriginalURL: "https://example.com/a", RedirectURL: "https://cdn.example.com/a",
			PageURL: "https://example.com/page", Referrer: "https://example.com/", Method: "POST",
			Headers: map[string]string{"X-Token": "1"}, ResolverKind: ResolverBrowser, CookieDomains: []string{"example.com"},
		}, false},
		{"missing current", Source{}, true},
		{"blank current", Source{CurrentURL: "  "}, true},
		{"relative current", Source{CurrentURL: "example.com/a"}, true},
		{"unparseable", Source{CurrentURL: "http://[::1"}, true},
		{"relative page", Source{CurrentURL: "https://a/b", PageURL: "/page"}, true},
		{"bad method", Source{CurrentURL: "https://a/b", Method: "GE T"}, true},
		{"bad header name", Source{CurrentURL: "https://a/b", Headers: map[string]string{"a b": "1"}}, true},
		{"header injection", Source{CurrentURL: "https://a/b", Headers: map[string]string{"A": "1\r\nB: 2"}}, true},
		{"ua injection", Source{CurrentURL: "https://a/b", UserAgent: "x\ny"}, true},
		{"empty cookie domain", Source{CurrentURL: "https://a/b", CookieDomains: []string{""}}, true},
		{"unknown resolver", Source{CurrentURL: "https://a/b", ResolverKind: "magic"}, true},
		{"bad fingerprint size", Source{CurrentURL: "https://a/b", Fingerprint: Fingerprint{Size: -2}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.src.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestFingerprintMatches(t *testing.T) {
	tests := []struct {
		name string
		a, b Fingerprint
		want bool
	}{
		{"both empty", Fingerprint{}, Fingerprint{}, true},
		{"same size", Fingerprint{Size: 10}, Fingerprint{Size: 10}, true},
		{"size differs", Fingerprint{Size: 10}, Fingerprint{Size: 11}, false},
		{"one size unknown", Fingerprint{Size: 10}, Fingerprint{Size: -1}, true},
		{"zero size is unknown", Fingerprint{Size: 10}, Fingerprint{}, true},
		{"same etag", Fingerprint{ETag: `"abc"`}, Fingerprint{ETag: `"abc"`}, true},
		{"etag differs", Fingerprint{ETag: `"abc"`}, Fingerprint{ETag: `"def"`}, false},
		{"one etag missing", Fingerprint{ETag: `"abc"`}, Fingerprint{}, true},
		{"weak etag ignored", Fingerprint{ETag: `W/"abc"`}, Fingerprint{ETag: `"def"`}, true},
		{"filename case-insensitive", Fingerprint{Filename: "Movie.MKV"}, Fingerprint{Filename: "movie.mkv"}, true},
		{"filename differs", Fingerprint{Filename: "a.zip"}, Fingerprint{Filename: "b.zip"}, false},
		{"one filename missing", Fingerprint{Filename: "a.zip"}, Fingerprint{}, true},
		{"content type ignored", Fingerprint{ContentType: "video/mp4"}, Fingerprint{ContentType: "text/html"}, true},
		{"all equal", Fingerprint{Size: 5, ETag: `"x"`, Filename: "f"}, Fingerprint{Size: 5, ETag: `"x"`, Filename: "F"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := tt.a.Matches(tt.b)
			if got != tt.want {
				t.Fatalf("Matches = %v (%s), want %v", got, reason, tt.want)
			}
			if got != (reason == "") {
				t.Fatalf("reason %q inconsistent with result %v", reason, got)
			}
			if back, _ := tt.b.Matches(tt.a); back != got {
				t.Fatal("Matches must be symmetric")
			}
		})
	}
}
