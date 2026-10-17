package browser

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Candidate is a download found while a page ran.
type Candidate struct {
	URL  string `json:"url"`
	Kind Kind   `json:"kind"`
	MIME string `json:"mime,omitempty"`
	// Size in bytes, -1 when unknown.
	Size int64 `json:"size"`
	// Filename is the name announced by the server or the browser.
	Filename string `json:"filename,omitempty"`
	// Source is "network" (a media or file response), "download" (the
	// browser started a download) or "blob" (a download of a blob: URL, which
	// only exists inside the browser).
	Source string `json:"source"`
	// Page is the document that triggered the request.
	Page string `json:"page,omitempty"`
	// Headers are the request headers worth repeating when the URL is
	// downloaded outside the browser (Referer, Origin).
	Headers map[string]string `json:"headers,omitempty"`
	FoundAt time.Time         `json:"foundAt"`
	// File is the path of the file the browser saved, for downloads that
	// completed (always set for blobs). The caller owns and must move or
	// delete it; Sniffer.Close removes the rest of the directory.
	File string `json:"file,omitempty"`
}

// SnifferOptions configures StartSniffer.
type SnifferOptions struct {
	// DownloadDir receives the files the browser downloads. When empty the
	// sniffer makes a temporary directory and Close removes it.
	DownloadDir string
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// maxPendingRequests bounds the request headers remembered between a request
// and its response.
const maxPendingRequests = 4096

// Sniffer records the downloads a tab runs into: media and file responses,
// and downloads the browser starts, including blob: downloads, which it saves
// to disk. Start it before navigating. Downloads are set per browser, so two
// sniffers on the same browser (the same site group) should not run at once.
type Sniffer struct {
	clock  func() time.Time
	dir    string
	ownDir bool

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	cands    []*Candidate
	byURL    map[string]*Candidate
	byGUID   map[string]*Candidate
	requests map[network.RequestID]requestInfo
	pending  int           // downloads started and not finished
	activity chan struct{} // receives a signal for every change (buffered, never blocks the producer)
	lastSeen time.Time
}

type requestInfo struct {
	headers map[string]string
	page    string
}

// StartSniffer enables network and download events on the tab of ctx and
// begins recording. Close it when done.
func StartSniffer(ctx context.Context, opts SnifferOptions) (*Sniffer, error) {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	s := &Sniffer{
		clock: clock, dir: opts.DownloadDir,
		byURL: map[string]*Candidate{}, byGUID: map[string]*Candidate{}, requests: map[network.RequestID]requestInfo{},
		activity: make(chan struct{}, 1), lastSeen: clock(),
	}
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "dlm-sniff-")
		if err != nil {
			return nil, fmt.Errorf("browser: sniffer directory: %w", err)
		}
		s.dir, s.ownDir = dir, true
	}
	ectx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Subscribe before enabling anything so no event is lost.
	reqs := chromedp.Events(ectx, network.RequestWillBeSent)
	resps := chromedp.Events(ectx, network.ResponseReceived)
	begins := chromedp.BrowserEvents(ectx, browser.DownloadWillBegin)
	progress := chromedp.BrowserEvents(ectx, browser.DownloadProgress)

	if _, err := chromedp.Call(ectx, network.Enable, network.EnableParams{}); err != nil {
		cancel()
		s.removeDir()
		return nil, fmt.Errorf("browser: enable network events: %w", err)
	}
	yes := true
	if _, err := chromedp.CallBrowser(ectx, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{
		Behavior: browser.SetDownloadBehaviorBehaviorAllowAndName, DownloadPath: s.dir, EventsEnabled: &yes,
	}); err != nil {
		cancel()
		s.removeDir()
		return nil, fmt.Errorf("browser: enable download events: %w", err)
	}

	s.wg.Add(4)
	go func() {
		defer s.wg.Done()
		for ev, err := range reqs {
			if err != nil {
				return
			}
			s.onRequest(ev)
		}
	}()
	go func() {
		defer s.wg.Done()
		for ev, err := range resps {
			if err != nil {
				return
			}
			s.onResponse(ev)
		}
	}()
	go func() {
		defer s.wg.Done()
		for ev, err := range begins {
			if err != nil {
				return
			}
			s.onDownloadBegin(ev)
		}
	}()
	go func() {
		defer s.wg.Done()
		for ev, err := range progress {
			if err != nil {
				return
			}
			s.onDownloadProgress(ev)
		}
	}()
	return s, nil
}

// Candidates returns a copy of what was found so far, oldest first.
func (s *Sniffer) Candidates() []Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Candidate, len(s.cands))
	for i, c := range s.cands {
		out[i] = *c
		out[i].Headers = cloneMap(c.Headers)
	}
	return out
}

// Wait blocks until nothing new was found and no download is running for
// quiet, or limit passed, or ctx ended. It returns the candidates found.
func (s *Sniffer) Wait(ctx context.Context, quiet, limit time.Duration) []Candidate {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		idle := s.pending == 0
		since := s.clock().Sub(s.lastSeen)
		s.mu.Unlock()
		wait := quiet
		if idle && since >= quiet {
			return s.Candidates()
		}
		if idle {
			wait = quiet - since
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return s.Candidates()
		case <-deadline.C:
			t.Stop()
			return s.Candidates()
		case <-s.activity:
			t.Stop()
		case <-t.C:
		}
	}
}

// Close stops recording and removes the temporary download directory (files
// the caller moved away are unaffected). It is idempotent.
func (s *Sniffer) Close() {
	s.cancel()
	s.wg.Wait()
	s.removeDir()
}

func (s *Sniffer) removeDir() {
	if s.ownDir {
		_ = os.RemoveAll(s.dir) // best effort: it is a temporary directory
	}
}

func (s *Sniffer) touch() {
	s.lastSeen = s.clock()
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *Sniffer) onRequest(ev network.EventRequestWillBeSent) {
	if ev.Request == nil {
		return
	}
	info := requestInfo{page: ev.DocumentURL, headers: map[string]string{}}
	for k, v := range ev.Request.Headers {
		switch strings.ToLower(k) {
		case "referer", "origin":
			info.headers[http.CanonicalHeaderKey(k)] = fmt.Sprint(v)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) >= maxPendingRequests {
		clear(s.requests) // a page with that many requests: forget the oldest rather than grow
	}
	s.requests[ev.RequestID] = info
}

func (s *Sniffer) onResponse(ev network.EventResponseReceived) {
	r := ev.Response
	if r == nil {
		return
	}
	h := lowerHeaders(r.Headers)
	ct := h["content-type"]
	if ct == "" {
		ct = r.MimeType
	}
	kind, ok := classify(r.URL, string(ev.Type), ct, h["content-disposition"], h["content-range"], int(r.Status))
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.requests[ev.RequestID]
	delete(s.requests, ev.RequestID)
	if !ok {
		return
	}
	size := int64(-1)
	if total := totalFromRange(h["content-range"]); total >= 0 {
		size = total
	} else if n, err := strconv.ParseInt(h["content-length"], 10, 64); err == nil && n >= 0 {
		size = n
	}
	key := dedupeKey(r.URL)
	if existing := s.byURL[key]; existing != nil {
		if existing.Size < 0 {
			existing.Size = size
		}
		return
	}
	c := &Candidate{
		URL: r.URL, Kind: kind, MIME: mediaType(ct), Size: size, Filename: dispositionFilename(h["content-disposition"]),
		Source: "network", Page: info.page, Headers: info.headers, FoundAt: s.clock(),
	}
	s.cands = append(s.cands, c)
	s.byURL[key] = c
	s.touch()
}

func (s *Sniffer) onDownloadBegin(ev browser.EventDownloadWillBegin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	source := "download"
	if strings.HasPrefix(ev.URL, "blob:") {
		source = "blob"
	}
	key := dedupeKey(ev.URL)
	c := s.byURL[key]
	if c == nil {
		c = &Candidate{URL: ev.URL, Kind: KindFile, Size: -1, FoundAt: s.clock()}
		s.cands = append(s.cands, c)
		s.byURL[key] = c
	}
	// A download that began as a network candidate keeps its kind; the
	// browser is saving it, so it is also available as a local file.
	c.Source, c.Filename = source, cmpOr(ev.SuggestedFilename, c.Filename)
	s.byGUID[ev.GUID] = c
	s.pending++
	s.touch()
}

func (s *Sniffer) onDownloadProgress(ev browser.EventDownloadProgress) {
	if ev.State == browser.DownloadProgressStateInProgress {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byGUID[ev.GUID]
	if c == nil {
		return
	}
	delete(s.byGUID, ev.GUID)
	s.pending--
	if ev.State == browser.DownloadProgressStateCompleted {
		file := ev.FilePath
		if file == "" {
			file = filepath.Join(s.dir, ev.GUID)
		}
		c.File = file
		if ev.TotalBytes > 0 {
			c.Size = int64(ev.TotalBytes)
		}
	}
	s.touch()
}

// dedupeKey identifies a resource by its URL without the fragment.
func dedupeKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	return u.String()
}

func lowerHeaders(h network.Headers) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = fmt.Sprint(v)
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return maps.Clone(m)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
