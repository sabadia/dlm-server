package cookies_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func ptr[T any](v T) *T { return &v }

func TestParseAndDetectFormat(t *testing.T) {
	for in, want := range map[string]cookies.Format{"": cookies.FormatJSON, "JSON": cookies.FormatJSON, "netscape": cookies.FormatNetscape} {
		got, err := cookies.ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := cookies.ParseFormat("xml"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("ParseFormat(xml) = %v, want ErrInvalidArgument", err)
	}
	if cookies.DetectFormat([]byte("  [{}]")) != cookies.FormatJSON || cookies.DetectFormat([]byte("{\"cookies\":[]}")) != cookies.FormatJSON {
		t.Error("JSON not detected")
	}
	if cookies.DetectFormat([]byte("# Netscape HTTP Cookie File\n")) != cookies.FormatNetscape {
		t.Error("Netscape not detected")
	}
}

func TestEncodeNetscape(t *testing.T) {
	exp := time.Unix(1_900_000_000, 0).UTC()
	got := string(cookies.EncodeNetscape([]*domain.Cookie{
		{Domain: "example.com", Name: "a", Value: "1", Path: "/", HostOnly: true},
		{Domain: "example.com", Name: "b", Value: "2", Path: "/x", Secure: true, HTTPOnly: true, ExpiresAt: &exp},
		{Domain: "example.com", Name: "evil", Value: "x\nsneaky.com\tTRUE\t/\tFALSE\t0\tinjected\tv", Path: "/"},
	}))
	want := "example.com\tFALSE\t/\tFALSE\t0\ta\t1\n" +
		"#HttpOnly_.example.com\tTRUE\t/x\tTRUE\t1900000000\tb\t2\n"
	if !strings.HasPrefix(got, "# Netscape HTTP Cookie File\n") {
		t.Errorf("missing header:\n%s", got)
	}
	if body := got[strings.Index(got, "\n\n")+2:]; body != want {
		t.Errorf("body =\n%q\nwant\n%q", body, want)
	}
}

func TestDecodeNetscape(t *testing.T) {
	in := "# Netscape HTTP Cookie File\r\n" +
		"\r\n" +
		"#HttpOnly_.example.com\tTRUE\t/\tTRUE\t1900000000\tsid\tabc\r\n" +
		"example.org\tFALSE\t/p\tFALSE\t0\tempty\t\r\n" +
		"example.org\tFALSE\t/p\tFALSE\t0\tnovalue\r\n" + // value and its tab missing (6 fields)
		"too\tfew\r\n" +
		"example.net\tFALSE\t/\tFALSE\tsoon\tbad\tv\r\n"
	got := cookies.DecodeNetscape([]byte(in))
	if len(got) != 5 {
		t.Fatalf("parsed %d lines, want 5: %+v", len(got), got)
	}
	sid := got[0].Cookie
	if got[0].Err != nil || sid.Domain != ".example.com" || sid.HostOnly || !sid.Secure || !sid.HTTPOnly ||
		sid.Name != "sid" || sid.Value != "abc" || sid.ExpiresAt == nil || sid.ExpiresAt.Unix() != 1_900_000_000 || got[0].Index != 3 {
		t.Errorf("sid = %+v (err %v, line %d)", sid, got[0].Err, got[0].Index)
	}
	if c := got[1].Cookie; got[1].Err != nil || c.Value != "" || !c.HostOnly || c.ExpiresAt != nil {
		t.Errorf("empty = %+v, %v", c, got[1].Err)
	}
	if got[2].Err != nil || got[2].Cookie.Name != "novalue" {
		t.Errorf("novalue = %+v, %v", got[2].Cookie, got[2].Err)
	}
	if got[3].Err == nil || got[4].Err == nil {
		t.Error("malformed lines were accepted")
	}
}

func TestDecodeJSONBrowserExport(t *testing.T) {
	in := `{"cookies": [
	  {"domain": ".example.com", "name": "a", "value": "1", "path": "/", "secure": true, "httpOnly": true,
	   "sameSite": "no_restriction", "expirationDate": 1900000000.5, "hostOnly": false,
	   "partitionKey": {"topLevelSite": "https://site.test"}},
	  {"domain": "example.org", "name": "b", "value": "2", "sameSite": "unspecified", "session": true, "expirationDate": 1900000000},
	  {"domain": "example.net", "name": "c", "value": "3", "expiresAt": "2030-01-02T03:04:05Z", "partitionKey": "https://p.test"},
	  {"name": 5}
	]}`
	got, err := cookies.DecodeJSON([]byte(in))
	if err != nil || len(got) != 4 {
		t.Fatalf("DecodeJSON = %d, %v", len(got), err)
	}
	a := got[0].Cookie
	if a.HostOnly || a.SameSite != "none" || a.PartitionKey != "https://site.test" || a.ExpiresAt == nil || a.ExpiresAt.UnixMilli() != 1_900_000_000_500 {
		t.Errorf("a = %+v", a)
	}
	if b := got[1].Cookie; !b.HostOnly || b.SameSite != "" || b.ExpiresAt != nil {
		t.Errorf("b = %+v (host-only inferred, session)", b)
	}
	if c := got[2].Cookie; c.PartitionKey != "https://p.test" || c.ExpiresAt == nil || c.ExpiresAt.Year() != 2030 {
		t.Errorf("c = %+v", c)
	}
	if got[3].Err == nil {
		t.Error("cookie with a numeric name was accepted")
	}
	if _, err := cookies.DecodeJSON([]byte(`"nope"`)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("DecodeJSON(string) = %v, want ErrInvalidArgument", err)
	}
	// A huge expirationDate is clamped, not overflowed.
	huge, err := cookies.DecodeJSON([]byte(`[{"domain":"x.test","name":"n","expirationDate":1e300}]`))
	if err != nil || huge[0].Cookie.ExpiresAt == nil || huge[0].Cookie.ExpiresAt.Year() != 9999 {
		t.Errorf("huge expiry = %+v, %v", huge[0].Cookie, err)
	}
}

func TestImportExportRoundTrip(t *testing.T) {
	for _, format := range []cookies.Format{cookies.FormatJSON, cookies.FormatNetscape} {
		t.Run(string(format), func(t *testing.T) {
			src := newFixture(t)
			ctx := context.Background()
			exp := t0.Add(48 * time.Hour)
			for _, c := range []*domain.Cookie{
				{Domain: "example.com", Name: "sid", Value: "abc=def", Path: "/", Secure: true, HTTPOnly: true, ExpiresAt: &exp},
				{Domain: "www.example.com", Name: "host", Value: "1", Path: "/p", HostOnly: true},
			} {
				if err := src.store.Put(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			all, err := src.store.List(ctx, storage.CookieFilter{})
			if err != nil {
				t.Fatal(err)
			}
			data, err := cookies.Encode(format, all)
			if err != nil {
				t.Fatal(err)
			}

			dst := newFixture(t)
			res, err := dst.store.Import(ctx, "", data) // detected
			if err != nil || res.Imported != 2 || res.Skipped != 0 || len(res.Errors) != 0 {
				t.Fatalf("Import = %+v, %v", res, err)
			}
			got, err := dst.store.List(ctx, storage.CookieFilter{})
			if err != nil || len(got) != 2 {
				t.Fatalf("List = %d, %v", len(got), err)
			}
			for i := range got {
				w, g := all[i], got[i]
				w2 := *w
				g2 := *g
				w2.ID, g2.ID, w2.CreatedAt, g2.CreatedAt, w2.UpdatedAt, g2.UpdatedAt = "", "", time.Time{}, time.Time{}, time.Time{}, time.Time{}
				w2.ExpiresAt, g2.ExpiresAt = trunc(w2.ExpiresAt), trunc(g2.ExpiresAt)
				if diff := cmp.Diff(&w2, &g2); diff != "" {
					t.Errorf("cookie %d differs after round trip (-want +got):\n%s", i, diff)
				}
			}
		})
	}
}

// trunc drops sub-second precision, which Netscape files cannot hold.
func trunc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return ptr(t.Truncate(time.Second))
}

func TestImportReportsProblems(t *testing.T) {
	f := newFixture(t)
	in := `[
	  {"domain": "example.com", "name": "ok", "value": "1"},
	  {"domain": "com", "name": "tld", "value": "1", "hostOnly": false},
	  {"domain": "example.com", "name": "", "value": "1"},
	  {"domain": "example.com", "name": "old", "value": "1", "expiresAt": "2000-01-01T00:00:00Z"},
	  {"domain": "example.com", "name": "badsite", "value": "1", "sameSite": "sometimes"},
	  {"domain": "localhost", "name": "local", "value": "1"}
	]`
	res, err := f.store.Import(context.Background(), cookies.FormatJSON, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 2 || res.Skipped != 1 || len(res.Errors) != 3 {
		t.Fatalf("Import = %+v, want 2 imported, 1 skipped, 3 errors", res)
	}
	for _, want := range []string{"#2 (tld)", "#3", "#5 (badsite)"} {
		if !slices.ContainsFunc(res.Errors, func(e string) bool { return strings.HasPrefix(e, want) }) {
			t.Errorf("no error for %q in %q", want, res.Errors)
		}
	}
	if _, err := f.store.Import(context.Background(), cookies.FormatJSON, []byte("not json")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Import(not json) = %v, want ErrInvalidArgument", err)
	}
}

func TestImportedCookiesReachTheJar(t *testing.T) {
	f := newFixture(t)
	data := "example.com\tTRUE\t/\tFALSE\t0\tsid\tfrom-file\n"
	if _, err := f.store.Import(context.Background(), cookies.FormatNetscape, []byte(data)); err != nil {
		t.Fatal(err)
	}
	if got := names(f.store.Cookies(mustURL(t, "https://api.example.com/x"))); !slices.Equal(got, []string{"sid=from-file"}) {
		t.Errorf("jar cookies = %v", got)
	}
}

func TestWriteTempNetscape(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := cookies.WriteTempNetscape(dir, []*domain.Cookie{{Domain: "example.com", Name: "a", Value: "1", Path: "/", HostOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "example.com\tFALSE\t/\tFALSE\t0\ta\t1\n") {
		t.Errorf("file = %q", data)
	}
	cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after cleanup: %v", err)
	}
}
