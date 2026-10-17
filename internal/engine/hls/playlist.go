package hls

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine/segmented"
)

// variant is one stream of a master playlist.
type variant struct {
	url       string
	bandwidth int64
	width     int
	height    int
	codecs    string
	name      string
	audio     string // audio rendition group
}

// rendition is an EXT-X-MEDIA audio track with its own playlist.
type rendition struct {
	url      string
	name     string
	language string
	group    string
	isDef    bool
}

type master struct {
	variants []variant
	audio    []rendition
}

// playlist is what parse found: exactly one of master and media is set.
type playlist struct {
	master *master
	media  *segmented.Media
}

var (
	errNotPlaylist = errors.New("hls: not an HLS playlist")
	errSampleAES   = errors.New("hls: SAMPLE-AES and other DRM encryption are not supported")
)

// parse decodes a playlist fetched from base. Every URL in the result is
// absolute.
func parse(data []byte, base *url.URL) (*playlist, error) {
	p, typ, err := m3u8.Decode(*bytes.NewBuffer(data), false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNotPlaylist, err)
	}
	switch typ {
	case m3u8.MASTER:
		mp, ok := p.(*m3u8.MasterPlaylist)
		if !ok {
			return nil, errNotPlaylist
		}
		return &playlist{master: toMaster(mp, base)}, nil
	case m3u8.MEDIA:
		mp, ok := p.(*m3u8.MediaPlaylist)
		if !ok {
			return nil, errNotPlaylist
		}
		m, err := toMedia(mp, base)
		if err != nil {
			return nil, err
		}
		return &playlist{media: m}, nil
	default:
		return nil, errNotPlaylist
	}
}

func resolve(base *url.URL, ref string) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

func toMaster(mp *m3u8.MasterPlaylist, base *url.URL) *master {
	m := &master{}
	seenAudio := map[string]bool{}
	for _, v := range mp.Variants {
		if v == nil || v.Iframe || v.URI == "" {
			continue
		}
		w, h := parseResolution(v.Resolution)
		m.variants = append(m.variants, variant{
			url: resolve(base, v.URI), bandwidth: int64(v.Bandwidth), width: w, height: h,
			codecs: v.Codecs, name: v.Name, audio: v.Audio,
		})
		for _, a := range v.Alternatives {
			if a == nil || !strings.EqualFold(a.Type, "AUDIO") || a.URI == "" {
				continue
			}
			key := a.GroupId + "\x00" + a.URI
			if seenAudio[key] {
				continue
			}
			seenAudio[key] = true
			m.audio = append(m.audio, rendition{url: resolve(base, a.URI), name: a.Name, language: a.Language, group: a.GroupId, isDef: a.Default})
		}
	}
	return m
}

func parseResolution(s string) (w, h int) {
	ws, hs, ok := strings.Cut(strings.ToLower(s), "x")
	if !ok {
		return 0, 0
	}
	w, _ = strconv.Atoi(ws) //nolint:errcheck // a malformed resolution counts as unknown
	h, _ = strconv.Atoi(hs) //nolint:errcheck // a malformed resolution counts as unknown
	return w, h
}

// toMedia normalises a media playlist: nil padding is dropped, URLs become
// absolute, byte ranges without an offset continue the previous range of the
// same resource, and the key and init section in force are attached to every
// segment.
func toMedia(mp *m3u8.MediaPlaylist, base *url.URL) (*segmented.Media, error) {
	m := &segmented.Media{Closed: mp.Closed, Target: float64(mp.TargetDuration)}
	curKey := currentKey(mp.Keys, base)
	var curMap *segmented.FileRef
	if mp.Map != nil {
		curMap = mapRef(mp.Map, base)
	}
	prevEnd := map[string]int64{} // resource URL -> end of its last byte range
	for _, s := range mp.Segments {
		if s == nil {
			continue
		}
		if len(s.Keys) > 0 {
			curKey = currentKey(s.Keys, base)
		}
		if s.Map != nil {
			curMap = mapRef(s.Map, base)
		}
		if curKey != nil && curKey.Method != "AES-128" {
			return nil, fmt.Errorf("%w (%s)", errSampleAES, curKey.Method)
		}
		seg := segmented.Segment{Seq: s.SeqId, Duration: s.Duration, Key: curKey, Init: curMap, Disc: s.Discontinuity}
		seg.Ref.URL = resolve(base, s.URI)
		if s.Limit > 0 {
			off := s.Offset
			if off == 0 {
				if end, ok := prevEnd[seg.Ref.URL]; ok {
					off = end // "@offset" omitted: the range follows the previous one
				}
			}
			seg.Ref.Range = &segmented.ByteRange{Offset: off, Length: s.Limit}
			prevEnd[seg.Ref.URL] = off + s.Limit
		}
		m.Segments = append(m.Segments, seg)
	}
	return m, nil
}

func mapRef(mp *m3u8.Map, base *url.URL) *segmented.FileRef {
	r := &segmented.FileRef{URL: resolve(base, mp.URI)}
	if mp.Limit > 0 {
		r.Range = &segmented.ByteRange{Offset: mp.Offset, Length: mp.Limit}
	}
	return r
}

// currentKey picks the identity key of a list of EXT-X-KEY tags; nil for
// METHOD=NONE or no key. A method other than AES-128 is kept so the caller can
// reject it.
func currentKey(keys []m3u8.Key, base *url.URL) *segmented.Key {
	for _, k := range keys {
		if kf := strings.ToLower(k.Keyformat); kf != "" && kf != "identity" {
			continue
		}
		method := strings.ToUpper(strings.TrimSpace(k.Method))
		if method == "" || method == "NONE" {
			return nil
		}
		ki := &segmented.Key{Method: method, URL: resolve(base, k.URI)}
		if iv, err := parseIV(k.IV); err == nil {
			ki.IV = iv
		}
		return ki
	}
	return nil
}

// parseIV reads an EXT-X-KEY IV: 0x followed by up to 32 hex digits.
func parseIV(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no IV")
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) > 32 {
		return nil, fmt.Errorf("IV %q is longer than 16 bytes", s)
	}
	s = strings.Repeat("0", 32-len(s)) + s
	iv := make([]byte, 16)
	for i := range iv {
		b, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("IV: %w", err)
		}
		iv[i] = byte(b)
	}
	return iv, nil
}

// ---- variant selection ----

var heightHint = regexp.MustCompile(`^(\d{3,4})p?$`)

// selectVariant picks a variant by a media hint: "" or "best" (highest
// bandwidth), "worst", a height such as "720" or "1080p" (the tallest variant
// not above it, else the lowest above it), or a regular expression (else
// substring) matched against the variant URL and name.
func selectVariant(vs []variant, hint string) (variant, error) {
	if len(vs) == 0 {
		return variant{}, errors.New("hls: the master playlist has no streams")
	}
	hint = strings.TrimSpace(hint)
	best := func(less func(a, b variant) bool) variant {
		out := vs[0]
		for _, v := range vs[1:] {
			if less(v, out) {
				out = v
			}
		}
		return out
	}
	switch strings.ToLower(hint) {
	case "", "best":
		return best(func(a, b variant) bool { return a.bandwidth > b.bandwidth }), nil
	case "worst":
		return best(func(a, b variant) bool { return a.bandwidth < b.bandwidth }), nil
	}
	if m := heightHint.FindStringSubmatch(hint); m != nil {
		want, _ := strconv.Atoi(m[1]) //nolint:errcheck // the pattern guarantees digits
		var under, over *variant
		for i := range vs {
			v := &vs[i]
			switch {
			case v.height == 0:
			case v.height <= want && (under == nil || v.height > under.height || (v.height == under.height && v.bandwidth > under.bandwidth)):
				under = v
			case v.height > want && (over == nil || v.height < over.height || (v.height == over.height && v.bandwidth > over.bandwidth)):
				over = v
			}
		}
		switch {
		case under != nil:
			return *under, nil
		case over != nil:
			return *over, nil
		}
		return variant{}, domain.InvalidArgument("hls: no stream has a known resolution to match %q", hint)
	}
	re, reErr := regexp.Compile(hint)
	for _, v := range vs {
		for _, s := range []string{v.url, v.name} {
			if s == "" {
				continue
			}
			if (reErr == nil && re.MatchString(s)) || (reErr != nil && strings.Contains(s, hint)) {
				return v, nil
			}
		}
	}
	return variant{}, domain.InvalidArgument("hls: no stream matches the hint %q", hint)
}

// pickAudio returns the separate audio rendition the variant plays with: the
// default of its group, else the first. ok is false when the variant's audio
// is muxed into its own playlist.
func pickAudio(m *master, v variant) (rendition, bool) {
	if v.audio == "" {
		return rendition{}, false
	}
	var first *rendition
	for i := range m.audio {
		r := &m.audio[i]
		if r.group != v.audio {
			continue
		}
		if r.isDef {
			return *r, true
		}
		if first == nil {
			first = r
		}
	}
	if first != nil {
		return *first, true
	}
	return rendition{}, false
}
