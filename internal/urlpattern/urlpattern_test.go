package urlpattern

import (
	"errors"
	"slices"
	"testing"
)

func TestExpand(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"http://x/a.jpg", []string{"http://x/a.jpg"}},
		{"http://x/f[1-3].jpg", []string{"http://x/f1.jpg", "http://x/f2.jpg", "http://x/f3.jpg"}},
		{"http://x/f[001-003].jpg", []string{"http://x/f001.jpg", "http://x/f002.jpg", "http://x/f003.jpg"}},
		{"http://x/f[08-11]", []string{"http://x/f08", "http://x/f09", "http://x/f10", "http://x/f11"}},
		{"http://x/f[1-10:4]", []string{"http://x/f1", "http://x/f5", "http://x/f9"}},
		{"http://x/[a-c].txt", []string{"http://x/a.txt", "http://x/b.txt", "http://x/c.txt"}},
		{"http://x/[A-B]", []string{"http://x/A", "http://x/B"}},
		{"http://x/[1-2]/[a-b]", []string{"http://x/1/a", "http://x/1/b", "http://x/2/a", "http://x/2/b"}},
		{"http://x/f[7-7]", []string{"http://x/f7"}},
		// Not ranges: literal.
		{"http://[::1]:8080/f", []string{"http://[::1]:8080/f"}},
		{"http://x/[abc]", []string{"http://x/[abc]"}},
		{"http://x/[a-]", []string{"http://x/[a-]"}},
		{"http://x/[1-3:0]", []string{"http://x/[1-3:0]"}},
		{"http://x/unclosed[1-3", []string{"http://x/unclosed[1-3"}},
		{"", []string{""}},
	} {
		got, err := Expand(c.in, 0)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Expand(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestExpandErrors(t *testing.T) {
	if _, err := Expand("http://x/[5-1]", 0); err == nil {
		t.Error("a backwards range was accepted")
	}
	if _, err := Expand("http://x/[z-a]", 0); err == nil {
		t.Error("a backwards letter range was accepted")
	}
	if _, err := Expand("http://x/[1-100000]", 0); !errors.Is(err, ErrTooMany) {
		t.Errorf("huge range: %v", err)
	}
	if _, err := Expand("http://x/[1-200][1-200]", 10000); !errors.Is(err, ErrTooMany) {
		t.Errorf("product over the limit: %v", err)
	}
	if got, err := Expand("http://x/[1-100][1-100]", 10000); err != nil || len(got) != 10000 {
		t.Errorf("product at the limit: %d, %v", len(got), err)
	}
	if _, err := Expand("http://x/[1-5]", 3); !errors.Is(err, ErrTooMany) {
		t.Errorf("custom limit: %v", err)
	}
}

func TestHasPattern(t *testing.T) {
	for in, want := range map[string]bool{
		"http://x/f[1-3]": true, "http://x/[a-c]": true, "http://[::1]/": false, "http://x/f": false, "http://x/[5-1]": true,
	} {
		if got := HasPattern(in); got != want {
			t.Errorf("HasPattern(%q) = %v", in, got)
		}
	}
}
