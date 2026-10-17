package refresh

import (
	"strconv"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestSignedURLExpiry(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name string
		url  string
		want time.Time // zero: none found
	}{
		{"aws v4", "https://b.s3.amazonaws.com/f?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20260102T030405Z&X-Amz-Expires=3600&X-Amz-Signature=abc", at("2026-01-02T04:04:05Z")},
		{"aws v4 lower case", "https://x/f?x-amz-date=20260102T030405Z&x-amz-expires=60", at("2026-01-02T03:05:05Z")},
		{"aws v4 expires without date", "https://x/f?X-Amz-Expires=3600&X-Amz-Signature=a", time.Time{}},
		{"aws v4 bad date", "https://x/f?X-Amz-Date=yesterday&X-Amz-Expires=3600", time.Time{}},
		{"google v4", "https://storage.googleapis.com/b/o?X-Goog-Date=20260102T030405Z&X-Goog-Expires=900&X-Goog-Signature=a", at("2026-01-02T03:19:05Z")},
		{"s3 v2", "https://b.s3.amazonaws.com/f?AWSAccessKeyId=AK&Expires=1893456000&Signature=abc", at("2030-01-01T00:00:00Z")},
		{"cloudfront", "https://d.cloudfront.net/f?Expires=1893456000&Signature=abc&Key-Pair-Id=K", at("2030-01-01T00:00:00Z")},
		{"azure sas", "https://a.blob.core.windows.net/c/f?sv=2022-11-02&se=2030-01-01T00%3A00%3A00Z&sr=b&sig=abc", at("2030-01-01T00:00:00Z")},
		{"azure se without sig", "https://x/f?se=2030-01-01T00:00:00Z", time.Time{}},
		{"generic exp with token", "https://x/f?token=abc&exp=1893456000", at("2030-01-01T00:00:00Z")},
		{"generic expires with hash", "https://x/f?hash=abc&expires=1893456000", at("2030-01-01T00:00:00Z")},
		{"milliseconds", "https://x/f?sig=abc&exp=1893456000000", at("2030-01-01T00:00:00Z")},
		{"rfc3339 expiry", "https://x/f?sig=abc&expiry=2030-01-01T00:00:00Z", at("2030-01-01T00:00:00Z")},
		{"bare expires is ignored", "https://x/f?expires=1893456000", time.Time{}},
		{"bare exp is ignored", "https://x/f?exp=1893456000&page=2", time.Time{}},
		{"not a timestamp", "https://x/f?sig=abc&exp=12345", time.Time{}},
		{"far future garbage", "https://x/f?sig=abc&exp=99999999999999999", time.Time{}},
		{"earliest wins", "https://x/f?sig=abc&exp=1893456000&expires=1767225600&X-Amz-Date=20300101T000000Z&X-Amz-Expires=60", at("2026-01-01T00:00:00Z")},
		{"no query", "https://x/f.zip", time.Time{}},
		{"unparsable url", "://bad", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SignedURLExpiry(tt.url)
			if ok != !tt.want.IsZero() || !got.Equal(tt.want) {
				t.Errorf("SignedURLExpiry = %v, %v; want %v", got, ok, tt.want)
			}
		})
	}
}

func TestSourceExpiryAndExpiresWithin(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	soon := "https://x/f?sig=a&exp=" + itoa(now.Add(2*time.Minute).Unix())
	later := "https://x/f?sig=a&exp=" + itoa(now.Add(48*time.Hour).Unix())
	past := "https://x/f?sig=a&exp=" + itoa(now.Add(-time.Hour).Unix())

	src := domain.Source{CurrentURL: later, RedirectURL: soon}
	got, ok := SourceExpiry(src)
	if !ok || !got.Equal(now.Add(2*time.Minute)) {
		t.Errorf("SourceExpiry = %v, %v; want the redirect URL's earlier expiry", got, ok)
	}
	if _, ok := SourceExpiry(domain.Source{CurrentURL: "https://x/plain.zip"}); ok {
		t.Error("SourceExpiry found an expiry in a plain URL")
	}

	for _, tt := range []struct {
		name   string
		src    domain.Source
		margin time.Duration
		want   bool
	}{
		{"within margin", domain.Source{CurrentURL: soon}, 5 * time.Minute, true},
		{"outside margin", domain.Source{CurrentURL: soon}, time.Minute, false},
		{"far away", domain.Source{CurrentURL: later}, 5 * time.Minute, false},
		{"already expired", domain.Source{CurrentURL: past}, 0, true},
		{"no expiry", domain.Source{CurrentURL: "https://x/f"}, time.Hour, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpiresWithin(tt.src, now, tt.margin); got != tt.want {
				t.Errorf("ExpiresWithin = %v, want %v", got, tt.want)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
