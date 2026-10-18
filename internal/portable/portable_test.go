package portable

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var sample = []Entry{
	{URL: "https://a.example/f.iso", Mirrors: []string{"https://b.example/f.iso"}, Name: "f.iso", Dir: "/data", Referrer: "https://a.example/", UserAgent: "ua", Headers: map[string]string{"X-A": "1", "X-B": "2"}},
	{URL: "https://c.example/g.bin"},
}

func TestRoundTrip(t *testing.T) {
	for _, f := range []string{FormatJSON, FormatAria2} {
		out, err := Format(f, sample)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(f, out)
		if err != nil {
			t.Fatalf("%s: %v\n%s", f, err, out)
		}
		if diff := cmp.Diff(sample, got); diff != "" {
			t.Errorf("%s round trip differs (-want +got):\n%s\n%s", f, diff, out)
		}
	}
	out, _ := Format(FormatURLs, sample)
	got, err := Parse(FormatURLs, out)
	if err != nil || len(got) != 2 || got[0].URL != sample[0].URL || got[1].URL != sample[1].URL {
		t.Errorf("urls round trip = %+v, %v", got, err)
	}
}

func TestParseURLs(t *testing.T) {
	got, err := Parse(FormatURLs, "# comment\n\n  https://a/1  \r\nhttps://a/2\n")
	if err != nil || len(got) != 2 || got[0].URL != "https://a/1" || got[1].URL != "https://a/2" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestParseAria2(t *testing.T) {
	in := "# list\nhttps://a/f.zip\thttps://b/f.zip\thttps://c/f.zip\n  out=x.zip\n  dir=/tmp/d\n  referer=https://r/\n  split=5\n  max-connection-per-server=4\n  header=Cookie: a=b\n\nhttps://a/g\n"
	got, err := Parse(FormatAria2, in)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{URL: "https://a/f.zip", Mirrors: []string{"https://b/f.zip", "https://c/f.zip"}, Name: "x.zip", Dir: "/tmp/d", Referrer: "https://r/", Headers: map[string]string{"Cookie": "a=b"}},
		{URL: "https://a/g"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestParseJSONForms(t *testing.T) {
	for name, in := range map[string]string{
		"document": `{"version":1,"tasks":[{"url":"https://a/1"},{"url":"https://a/2","name":"n"}]}`,
		"array":    `[{"url":"https://a/1"},{"url":"https://a/2","name":"n"}]`,
	} {
		got, err := Parse(FormatJSON, in)
		if err != nil || len(got) != 2 || got[1].Name != "n" {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for name, c := range map[string]struct{ format, in string }{
		"unknown format":     {"xml", ""},
		"bad json":           {FormatJSON, `{"tasks":`},
		"entry without url":  {FormatJSON, `{"tasks":[{"name":"x"}]}`},
		"newer json":         {FormatJSON, `{"version":2,"tasks":[]}`},
		"aria2 stray option": {FormatAria2, "  out=x\n"},
		"aria2 no equals":    {FormatAria2, "https://a\n  out\n"},
		"aria2 bad header":   {FormatAria2, "https://a\n  header=nocolon\n"},
		"too large":          {FormatURLs, strings.Repeat("x", MaxBytes+1)},
		"too many":           {FormatURLs, strings.Repeat("https://a/x\n", MaxEntries+1)},
	} {
		if _, err := Parse(c.format, c.in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDetect(t *testing.T) {
	for in, want := range map[string]string{"x.JSON": FormatJSON, "x.aria2": FormatAria2, "urls.txt": FormatURLs, "x.bin": "", "": ""} {
		if got := Detect(in); got != want {
			t.Errorf("Detect(%q) = %q, want %q", in, got, want)
		}
	}
}
