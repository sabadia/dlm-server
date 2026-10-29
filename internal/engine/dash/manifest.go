package dash

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Eyevinn/dash-mpd/mpd"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine/segmented"
)

var (
	errNotManifest = errors.New("dash: not an MPD manifest")
	errDynamic     = errors.New("dash: live (dynamic) manifests are not supported")
	errDRM         = errors.New("dash: the stream is DRM protected (ContentProtection)")
	errRemote      = errors.New("dash: remote periods (xlink) are not supported")
)

// rep is one Representation with everything inherited from its Period and
// AdaptationSet resolved.
type rep struct {
	id        string
	kind      string // "video" or "audio"
	bandwidth int
	height    int
	codecs    string
	base      *url.URL
	tmpl      *mpd.SegmentTemplateType
	list      *mpd.SegmentListType
	sbase     *mpd.SegmentBaseType
}

type period struct {
	dur  float64 // seconds; 0 when unknown
	reps []rep
}

type manifest struct {
	periods []period
}

// parse reads an MPD fetched from at. Live manifests and DRM are refused.
func parse(data []byte, at *url.URL) (*manifest, error) {
	m, err := mpd.MPDFromBytes(data)
	if err != nil || m == nil || m.XMLName.Local != "MPD" {
		return nil, errNotManifest
	}
	if m.Type != nil && strings.EqualFold(*m.Type, "dynamic") {
		return nil, errDynamic
	}
	if len(m.ContentProtection) > 0 {
		return nil, errDRM
	}
	var total float64
	if m.MediaPresentationDuration != nil {
		total = m.MediaPresentationDuration.Seconds()
	}
	root := resolveBase(at, m.BaseURL)
	out := &manifest{}
	var cursor float64
	for i, p := range m.Periods {
		if p.XlinkHref != "" {
			return nil, errRemote
		}
		if len(p.ContentProtections) > 0 {
			return nil, errDRM
		}
		start := cursor
		if p.Start != nil {
			start = p.Start.Seconds()
		}
		dur := 0.0
		switch {
		case p.Duration != nil:
			dur = p.Duration.Seconds()
		case i+1 < len(m.Periods) && m.Periods[i+1].Start != nil:
			dur = m.Periods[i+1].Start.Seconds() - start
		case total > start:
			dur = total - start
		}
		cursor = start + dur
		pb := resolveBase(root, p.BaseURLs)
		pr := period{dur: dur}
		for _, as := range p.AdaptationSets {
			if len(as.ContentProtections) > 0 {
				return nil, errDRM
			}
			ab := resolveBase(pb, as.BaseURLs)
			for _, r := range as.Representations {
				if len(r.ContentProtections) > 0 {
					return nil, errDRM
				}
				kind := kindOf(as, r)
				if kind == "" {
					continue // subtitles, thumbnails
				}
				x := rep{
					id: r.Id, kind: kind, bandwidth: int(r.Bandwidth), height: int(r.Height),
					codecs: firstNonEmpty(r.Codecs, as.Codecs),
					base:   resolveBase(ab, r.BaseURLs),
					tmpl:   firstTemplate(r.SegmentTemplate, as.SegmentTemplate, p.SegmentTemplate),
					list:   firstList(r.SegmentList, as.SegmentList, p.SegmentList),
					sbase:  firstBase(r.SegmentBase, as.SegmentBase, p.SegmentBase),
				}
				if x.height == 0 {
					x.height = int(as.Height)
				}
				pr.reps = append(pr.reps, x)
			}
		}
		out.periods = append(out.periods, pr)
	}
	if len(out.periods) == 0 {
		return nil, errNotManifest
	}
	return out, nil
}

func kindOf(as *mpd.AdaptationSetType, r *mpd.RepresentationType) string {
	for _, s := range []string{string(as.ContentType), firstNonEmpty(r.MimeType, as.MimeType)} {
		s = strings.ToLower(s)
		switch {
		case strings.HasPrefix(s, "video"):
			return "video"
		case strings.HasPrefix(s, "audio"):
			return "audio"
		}
	}
	c := strings.ToLower(firstNonEmpty(r.Codecs, as.Codecs))
	switch {
	case strings.HasPrefix(c, "avc"), strings.HasPrefix(c, "hvc"), strings.HasPrefix(c, "hev"), strings.HasPrefix(c, "vp"), strings.HasPrefix(c, "av01"):
		return "video"
	case strings.HasPrefix(c, "mp4a"), strings.HasPrefix(c, "opus"), strings.HasPrefix(c, "ac-3"), strings.HasPrefix(c, "ec-3"):
		return "audio"
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstTemplate(ts ...*mpd.SegmentTemplateType) *mpd.SegmentTemplateType {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return nil
}

func firstList(ts ...*mpd.SegmentListType) *mpd.SegmentListType {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return nil
}

func firstBase(ts ...*mpd.SegmentBaseType) *mpd.SegmentBaseType {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return nil
}

// resolveBase applies the first BaseURL of a level to the base above it.
func resolveBase(base *url.URL, bs []*mpd.BaseURLType) *url.URL {
	if len(bs) == 0 || bs[0] == nil || strings.TrimSpace(string(bs[0].Value)) == "" {
		return base
	}
	ref, err := url.Parse(strings.TrimSpace(string(bs[0].Value)))
	if err != nil {
		return base
	}
	return base.ResolveReference(ref)
}

var heightHint = regexp.MustCompile(`^(\d{3,4})p?$`)

// selectRep picks a representation by a media hint: "" or "best" (highest
// bandwidth), "worst", a height such as "720" or "1080p" (the tallest not
// above it, else the lowest above it), or a regular expression (else
// substring) matched against the representation id and URL.
func selectRep(rs []rep, hint string) (rep, error) {
	if len(rs) == 0 {
		return rep{}, errors.New("dash: no representation to choose from")
	}
	hint = strings.TrimSpace(hint)
	best := func(less func(a, b rep) bool) rep {
		out := rs[0]
		for _, v := range rs[1:] {
			if less(v, out) {
				out = v
			}
		}
		return out
	}
	switch strings.ToLower(hint) {
	case "", "best":
		return best(func(a, b rep) bool { return a.bandwidth > b.bandwidth }), nil
	case "worst":
		return best(func(a, b rep) bool { return a.bandwidth < b.bandwidth }), nil
	}
	if m := heightHint.FindStringSubmatch(hint); m != nil {
		want, _ := strconv.Atoi(m[1]) //nolint:errcheck // the pattern guarantees digits
		var under, over *rep
		for i := range rs {
			v := &rs[i]
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
		return rep{}, domain.InvalidArgument("dash: no stream has a known resolution to match %q", hint)
	}
	re, reErr := regexp.Compile(hint)
	for _, v := range rs {
		for _, s := range []string{v.id, v.base.String()} {
			if s == "" {
				continue
			}
			if (reErr == nil && re.MatchString(s)) || (reErr != nil && strings.Contains(s, hint)) {
				return v, nil
			}
		}
	}
	return rep{}, domain.InvalidArgument("dash: no stream matches the hint %q", hint)
}

// trackMedia is the chosen stream of one kind across all periods.
type trackMedia struct {
	kind      string
	bandwidth int
	media     *segmented.Media
}

// tracks chooses the video (by hint) and audio representations of every period
// and lists their segments. An audio-only stream yields one audio track; a
// video representation that already carries audio codecs yields no separate
// audio track.
func (m *manifest) tracks(hint string) ([]trackMedia, error) {
	var out []trackMedia
	for _, kind := range []string{"video", "audio"} {
		tm := trackMedia{kind: kind, media: &segmented.Media{Closed: true}}
		var seq uint64
		found, muxed := false, false
		for pi, p := range m.periods {
			var cands []rep
			for _, r := range p.reps {
				if r.kind == kind {
					cands = append(cands, r)
				}
			}
			if len(cands) == 0 {
				continue
			}
			var r rep
			var err error
			if kind == "audio" {
				r, err = selectRep(cands, audioHint(hint))
			} else {
				r, err = selectRep(cands, hint)
			}
			if err != nil {
				return nil, err
			}
			segs, err := r.segments(p.dur, &seq)
			if err != nil {
				return nil, err
			}
			if len(segs) == 0 {
				continue
			}
			if found || pi > 0 {
				segs[0].Disc = true
			}
			found = true
			tm.bandwidth = max(tm.bandwidth, r.bandwidth)
			tm.media.Segments = append(tm.media.Segments, segs...)
			muxed = muxed || (kind == "video" && muxedAudio(r.codecs))
		}
		if !found {
			continue
		}
		for _, s := range tm.media.Segments {
			tm.media.Target = max(tm.media.Target, s.Duration)
		}
		out = append(out, tm)
		if muxed {
			break // the video already carries the audio
		}
	}
	return out, nil
}

// audioHint maps the video hint to the audio choice: only "worst" carries over.
func audioHint(hint string) string {
	if strings.EqualFold(strings.TrimSpace(hint), "worst") {
		return "worst"
	}
	return ""
}

// muxedAudio reports whether a codecs string lists both video and audio.
func muxedAudio(codecs string) bool {
	c := strings.ToLower(codecs)
	return strings.Contains(c, "mp4a") || strings.Contains(c, "opus") || strings.Contains(c, "ac-3") || strings.Contains(c, "ec-3")
}

// segments lists the segments of r for a period of dur seconds (0 unknown).
// seq is the running segment number of the track.
func (r rep) segments(dur float64, seq *uint64) ([]segmented.Segment, error) {
	switch {
	case r.tmpl != nil && (r.tmpl.Media != ""):
		return r.templateSegments(dur, seq)
	case r.list != nil && len(r.list.SegmentURL) > 0:
		return r.listSegments(dur, seq)
	default:
		return r.singleSegment(dur, seq)
	}
}

func (r rep) abs(ref string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("dash: bad URL %q: %w", ref, err)
	}
	return r.base.ResolveReference(u).String(), nil
}

func timescaleOf(sb *mpd.SegmentBaseType) float64 {
	if sb.Timescale != nil && *sb.Timescale > 0 {
		return float64(*sb.Timescale)
	}
	return 1
}

var templateID = regexp.MustCompile(`\$(RepresentationID|Number|Bandwidth|Time|SubNumber)?(%0\d+d)?\$`)

// expand fills the identifiers of a SegmentTemplate URL.
func expand(tmpl, id string, bandwidth int, number, tm uint64) (string, error) {
	var bad error
	out := templateID.ReplaceAllStringFunc(tmpl, func(s string) string {
		m := templateID.FindStringSubmatch(s)
		name, format := m[1], m[2]
		if name == "" {
			return "$" // "$$"
		}
		var v uint64
		switch name {
		case "RepresentationID":
			return id
		case "Bandwidth":
			v = uint64(max(bandwidth, 0)) //nolint:gosec // clamped non-negative
		case "Number":
			v = number
		case "Time":
			v = tm
		default:
			bad = fmt.Errorf("dash: template identifier $%s$ is not supported", name)
			return s
		}
		if format != "" {
			w, _ := strconv.Atoi(format[2 : len(format)-1]) //nolint:errcheck // the pattern guarantees digits
			return fmt.Sprintf("%0*d", w, v)
		}
		return strconv.FormatUint(v, 10)
	})
	return out, bad
}

func (r rep) templateSegments(dur float64, seq *uint64) ([]segmented.Segment, error) {
	t := r.tmpl
	scale := timescaleOf(&t.SegmentBaseType)
	start := uint64(1)
	if t.StartNumber != nil {
		start = uint64(*t.StartNumber)
	}
	var init *segmented.FileRef
	if t.Initialization != "" {
		u, err := expand(t.Initialization, r.id, r.bandwidth, 0, 0)
		if err != nil {
			return nil, err
		}
		if u, err = r.abs(u); err != nil {
			return nil, err
		}
		init = &segmented.FileRef{URL: u}
	}
	type item struct {
		tm uint64
		d  uint64
	}
	var items []item
	switch {
	case t.SegmentTimeline != nil && len(t.SegmentTimeline.S) > 0:
		var cur uint64
		for _, s := range t.SegmentTimeline.S {
			if s.T != nil {
				cur = *s.T
			}
			if s.D == 0 {
				return nil, engineFatal(errors.New("dash: a SegmentTimeline entry has no duration"))
			}
			n := s.R + 1
			if s.R < 0 { // repeat until the end of the period
				if dur <= 0 {
					return nil, engineFatal(errors.New("dash: an open-ended SegmentTimeline needs the period duration"))
				}
				n = max(int(math.Ceil((dur*scale-float64(cur))/float64(s.D))), 1)
			}
			for i := 0; i < n; i++ {
				items = append(items, item{cur, s.D})
				cur += s.D
			}
		}
	case t.Duration != nil && *t.Duration > 0:
		if dur <= 0 {
			return nil, engineFatal(errors.New("dash: the period duration is unknown, so the segment count cannot be computed"))
		}
		d := uint64(*t.Duration)
		n := int(math.Ceil(dur * scale / float64(d)))
		for i := 0; i < n; i++ {
			items = append(items, item{uint64(i) * d, d})
		}
	default:
		return nil, engineFatal(errors.New("dash: SegmentTemplate has neither a timeline nor a duration"))
	}
	if t.EndNumber != nil {
		if last := int(*t.EndNumber) - int(start) + 1; last >= 0 && last < len(items) {
			items = items[:last]
		}
	}
	segs := make([]segmented.Segment, 0, len(items))
	for i, it := range items {
		u, err := expand(t.Media, r.id, r.bandwidth, start+uint64(i), it.tm)
		if err != nil {
			return nil, engineFatal(err)
		}
		if u, err = r.abs(u); err != nil {
			return nil, engineFatal(err)
		}
		segs = append(segs, segmented.Segment{Seq: *seq, Ref: segmented.FileRef{URL: u}, Duration: float64(it.d) / scale, Init: init})
		*seq++
	}
	return segs, nil
}

func (r rep) listSegments(dur float64, seq *uint64) ([]segmented.Segment, error) {
	l := r.list
	scale := timescaleOf(&l.SegmentBaseType)
	var init *segmented.FileRef
	if l.Initialization != nil {
		// Without a sourceURL the section is a range of the BaseURL file.
		u, err := r.abs(firstNonEmpty(string(l.Initialization.SourceURL), r.base.String()))
		if err != nil {
			return nil, engineFatal(err)
		}
		rng, err := parseRange(l.Initialization.Range)
		if err != nil {
			return nil, engineFatal(err)
		}
		init = &segmented.FileRef{URL: u, Range: rng}
	}
	each := 0.0
	switch {
	case l.Duration != nil && *l.Duration > 0:
		each = float64(*l.Duration) / scale
	case dur > 0:
		each = dur / float64(len(l.SegmentURL))
	}
	segs := make([]segmented.Segment, 0, len(l.SegmentURL))
	for _, su := range l.SegmentURL {
		ref := string(su.Media)
		if ref == "" { // a range of the Representation's BaseURL
			ref = r.base.String()
		}
		u, err := r.abs(ref)
		if err != nil {
			return nil, engineFatal(err)
		}
		rng, err := parseRange(string(su.MediaRange))
		if err != nil {
			return nil, engineFatal(err)
		}
		segs = append(segs, segmented.Segment{Seq: *seq, Ref: segmented.FileRef{URL: u, Range: rng}, Duration: each, Init: init})
		*seq++
	}
	return segs, nil
}

// singleSegment handles a Representation that is one file (BaseURL alone, or
// with a SegmentBase index): the whole file is the only segment.
func (r rep) singleSegment(dur float64, seq *uint64) ([]segmented.Segment, error) {
	if r.base == nil {
		return nil, nil
	}
	s := segmented.Segment{Seq: *seq, Ref: segmented.FileRef{URL: r.base.String()}, Duration: dur}
	*seq++
	return []segmented.Segment{s}, nil
}

// parseRange reads "first-last"; empty means the whole resource.
func parseRange(s string) (*segmented.ByteRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil //nolint:nilnil // no range is not an error
	}
	a, b, ok := strings.Cut(s, "-")
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if !ok || err1 != nil || err2 != nil || first < 0 || last < first {
		return nil, fmt.Errorf("dash: bad byte range %q", s)
	}
	return &segmented.ByteRange{Offset: first, Length: last - first + 1}, nil
}

// formats lists the video representations of the first period for a picker,
// best first. A format's id selects it through Source.MediaHint: its height
// ("720p") when no other representation has that height, else an exact match
// on its representation id.
func (m *manifest) formats() []domain.Format {
	if len(m.periods) == 0 {
		return nil
	}
	var reps []rep
	for _, r := range m.periods[0].reps {
		if r.kind == "video" {
			reps = append(reps, r)
		}
	}
	heights := map[int]int{}
	for _, r := range reps {
		heights[r.height]++
	}
	out := make([]domain.Format, 0, len(reps))
	for _, r := range reps {
		id := "^" + regexp.QuoteMeta(r.id) + "$"
		if r.height > 0 && heights[r.height] == 1 {
			id = strconv.Itoa(r.height) + "p"
		}
		out = append(out, domain.Format{
			ID: id, Label: domain.FormatLabel(r.height, int64(r.bandwidth), heights[r.height] > 1), Kind: domain.FormatVideo,
			Height: r.height, Bandwidth: int64(r.bandwidth), Codecs: r.codecs, Size: -1,
		})
	}
	slices.SortStableFunc(out, func(a, b domain.Format) int {
		if a.Height != b.Height {
			return b.Height - a.Height
		}
		return int(b.Bandwidth - a.Bandwidth)
	})
	return out
}
