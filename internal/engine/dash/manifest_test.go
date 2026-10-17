package dash

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func mustParse(t *testing.T, xml string) *manifest {
	t.Helper()
	u, _ := url.Parse("https://cdn.example/v/show/manifest.mpd")
	m, err := parse([]byte(xml), u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

const timelineMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10S" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">
 <BaseURL>media/</BaseURL>
 <Period>
  <AdaptationSet contentType="video" mimeType="video/mp4">
   <SegmentTemplate timescale="1000" initialization="$RepresentationID$/init.mp4" media="$RepresentationID$/s$Number%05d$-$Time$-$Bandwidth$.m4s" startNumber="7">
    <SegmentTimeline><S t="0" d="4000" r="1"/><S d="2000"/></SegmentTimeline>
   </SegmentTemplate>
   <Representation id="v-low" bandwidth="500000" width="640" height="360" codecs="avc1.4d401e"/>
   <Representation id="v-high" bandwidth="2000000" width="1280" height="720" codecs="avc1.4d401f"/>
  </AdaptationSet>
  <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en">
   <SegmentTemplate timescale="48000" initialization="a/init.mp4" media="a/$Number$.m4s" duration="192000"/>
   <Representation id="a1" bandwidth="128000" codecs="mp4a.40.2"/>
  </AdaptationSet>
 </Period>
</MPD>`

func TestTimelineTemplateAndSelection(t *testing.T) {
	m := mustParse(t, timelineMPD)
	tms, err := m.tracks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(tms) != 2 || tms[0].kind != "video" || tms[1].kind != "audio" {
		t.Fatalf("tracks = %+v", tms)
	}
	v := tms[0].media.Segments
	if len(v) != 3 {
		t.Fatalf("video segments = %d, want 3", len(v))
	}
	want := "https://cdn.example/v/show/media/v-high/s00007-0-2000000.m4s"
	if v[0].Ref.URL != want {
		t.Errorf("first video URL = %q, want %q", v[0].Ref.URL, want)
	}
	if got := v[2].Ref.URL; !strings.HasSuffix(got, "s00009-8000-2000000.m4s") {
		t.Errorf("third video URL = %q", got)
	}
	if v[0].Init == nil || !strings.HasSuffix(v[0].Init.URL, "v-high/init.mp4") {
		t.Errorf("init = %+v", v[0].Init)
	}
	if v[2].Duration != 2 || tms[0].media.Target != 4 || !tms[0].media.Closed {
		t.Errorf("durations: last %v target %v closed %v", v[2].Duration, tms[0].media.Target, tms[0].media.Closed)
	}
	a := tms[1].media.Segments
	if len(a) != 3 { // 10 s at 4 s per segment
		t.Fatalf("audio segments = %d, want 3", len(a))
	}
	if a[1].Duration != 4 || !strings.HasSuffix(a[1].Ref.URL, "media/a/2.m4s") {
		t.Errorf("audio[1] = %+v", a[1])
	}

	low, err := m.tracks("worst")
	if err != nil || !strings.Contains(low[0].media.Segments[0].Ref.URL, "v-low") {
		t.Errorf("worst: %v %v", err, low)
	}
	h, err := m.tracks("360p")
	if err != nil || !strings.Contains(h[0].media.Segments[0].Ref.URL, "v-low") {
		t.Errorf("360p: %v", err)
	}
	if r, err := m.tracks("^v-h"); err != nil || !strings.Contains(r[0].media.Segments[0].Ref.URL, "v-high") {
		t.Errorf("regexp: %v", err)
	}
	if _, err := m.tracks("nomatch"); err == nil {
		t.Error("want an error for a hint that matches nothing")
	}
}

func TestRepeatToEndOfPeriodAndEndNumber(t *testing.T) {
	m := mustParse(t, `<MPD type="static" mediaPresentationDuration="PT9S"><Period>
	 <AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="1" media="s$Time$.m4s"><SegmentTimeline><S t="0" d="2" r="-1"/></SegmentTimeline></SegmentTemplate>
	  <Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`)
	tms, err := m.tracks("")
	if err != nil || len(tms[0].media.Segments) != 5 {
		t.Fatalf("segments = %v %v, want 5", len(tms[0].media.Segments), err)
	}
	m = mustParse(t, `<MPD type="static" mediaPresentationDuration="PT9S"><Period>
	 <AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="1" duration="3" startNumber="1" endNumber="2" media="s$Number$.m4s"/>
	  <Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`)
	tms, err = m.tracks("")
	if err != nil || len(tms[0].media.Segments) != 2 {
		t.Fatalf("endNumber: %v %v", len(tms[0].media.Segments), err)
	}
}

func TestSegmentListAndSingleFile(t *testing.T) {
	m := mustParse(t, `<MPD type="static" mediaPresentationDuration="PT6S"><Period>
	 <AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="1" codecs="avc1,mp4a.40.2">
	  <BaseURL>v.mp4</BaseURL>
	  <SegmentList duration="3"><Initialization range="0-99"/><SegmentURL mediaRange="100-199"/><SegmentURL mediaRange="200-299"/></SegmentList>
	 </Representation></AdaptationSet>
	 <AdaptationSet mimeType="audio/mp4"><Representation id="a" bandwidth="1"><BaseURL>a.m4a</BaseURL></Representation></AdaptationSet>
	</Period></MPD>`)
	tms, err := m.tracks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(tms) != 1 { // the video carries audio codecs: no separate audio track
		t.Fatalf("tracks = %d, want 1 (muxed audio)", len(tms))
	}
	s := tms[0].media.Segments
	if len(s) != 2 || s[0].Ref.Range == nil || s[0].Ref.Range.Offset != 100 || s[0].Ref.Range.Length != 100 || s[0].Duration != 3 {
		t.Fatalf("segments = %+v", s)
	}
	if s[0].Init == nil || s[0].Init.Range == nil || s[0].Init.Range.Length != 100 {
		t.Errorf("init = %+v", s[0].Init)
	}

	single := mustParse(t, `<MPD type="static" mediaPresentationDuration="PT6S"><Period>
	 <AdaptationSet mimeType="audio/mp4"><Representation id="a" bandwidth="1"><BaseURL>a.m4a</BaseURL><SegmentBase indexRange="0-10"/></Representation></AdaptationSet></Period></MPD>`)
	tms, err = single.tracks("")
	if err != nil || len(tms) != 1 || len(tms[0].media.Segments) != 1 || tms[0].kind != "audio" ||
		tms[0].media.Segments[0].Ref.URL != "https://cdn.example/v/show/a.m4a" {
		t.Fatalf("single = %+v %v", tms, err)
	}
}

func TestMultiplePeriodsAreConcatenated(t *testing.T) {
	m := mustParse(t, `<MPD type="static" mediaPresentationDuration="PT8S">
	 <Period id="1" duration="PT4S"><AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="1" duration="2" initialization="p1/i.mp4" media="p1/$Number$.m4s"/><Representation id="v" bandwidth="1"/></AdaptationSet></Period>
	 <Period id="2"><AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="1" duration="2" initialization="p2/i.mp4" media="p2/$Number$.m4s"/><Representation id="v" bandwidth="1"/></AdaptationSet></Period>
	</MPD>`)
	tms, err := m.tracks("")
	if err != nil {
		t.Fatal(err)
	}
	s := tms[0].media.Segments
	if len(s) != 4 {
		t.Fatalf("segments = %d, want 4", len(s))
	}
	for i, seg := range s {
		if seg.Seq != uint64(i) {
			t.Errorf("seg %d has Seq %d", i, seg.Seq)
		}
	}
	if s[0].Disc || !s[2].Disc || s[0].Init.URL == s[2].Init.URL {
		t.Errorf("period boundary: disc %v/%v inits %v %v", s[0].Disc, s[2].Disc, s[0].Init.URL, s[2].Init.URL)
	}
}

func TestRefusals(t *testing.T) {
	u, _ := url.Parse("https://x/m.mpd")
	for name, c := range map[string]struct {
		xml  string
		want error
	}{
		"dynamic": {`<MPD type="dynamic"><Period/></MPD>`, errDynamic},
		"drm":     {`<MPD type="static"><Period><AdaptationSet mimeType="video/mp4"><ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"/><Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`, errDRM},
		"xlink":   {`<MPD type="static" xmlns:xlink="http://www.w3.org/1999/xlink"><Period xlink:href="http://x/p.xml"/></MPD>`, errRemote},
		"html":    {`<!DOCTYPE html><html></html>`, errNotManifest},
		"junk":    {`nope`, errNotManifest},
	} {
		if _, err := parse([]byte(c.xml), u); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestExpand(t *testing.T) {
	got, err := expand("a/$RepresentationID$/$Number%03d$_$Time$_$$_$Bandwidth$.m4s", "r1", 99, 5, 1234)
	if err != nil || got != "a/r1/005_1234_$_99.m4s" {
		t.Errorf("expand = %q, %v", got, err)
	}
	if _, err := expand("$SubNumber$", "r", 1, 1, 1); err == nil {
		t.Error("want an error for $SubNumber$")
	}
}

func TestMatchAndSuggestName(t *testing.T) {
	e := New(engineEnv(), nil)
	if !e.Match(srcOf("https://x/a/manifest.MPD?t=1")) || e.Match(srcOf("https://x/a.m3u8")) || e.Match(srcOf("ftp://x/a.mpd")) {
		t.Error("Match mismatch")
	}
	for u, want := range map[string]string{
		"https://x/videos/my-show/manifest.mpd": "my-show.mp4",
		"https://cdn.example/Big%20Movie.mpd":   "Big Movie.mp4",
		"https://cdn.example/manifest.mpd":      "cdn.example.mp4",
	} {
		if got := suggestName(u); got != want {
			t.Errorf("suggestName(%q) = %q, want %q", u, got, want)
		}
	}
}
