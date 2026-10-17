package httpx

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// ProbeResult is the outcome of Probe. The response is kept open so the
// downloader can continue reading the body from offset 0 instead of issuing
// a second request (one-time URLs only serve a single request). The caller
// owns the result and must Close it, or hand it over (e.g. as an
// engine.Probe attachment) to an owner that will.
type ProbeResult struct {
	// Response is the open probe response; its body starts at offset 0 of
	// the resource. Its status is 2xx.
	Response *http.Response
	// Source is the probed source updated with the final URL after
	// redirects (RedirectURL) and the fingerprint.
	Source domain.Source
	// Resource describes the single file served at the URL.
	Resource domain.Resource
	// Filename is the detected, sanitised file name.
	Filename string
}

// Close closes the response body. It is safe to call more than once and on
// a nil result.
func (r *ProbeResult) Close() error {
	if r == nil || r.Response == nil || r.Response.Body == nil {
		return nil
	}
	return r.Response.Body.Close() //nolint:wrapcheck // body close error needs no extra context
}

// Probe requests src.CurrentURL with "Range: bytes=0-" and describes the
// resource: size, range support, file name and fingerprint. The response is
// left open in the result (see ProbeResult).
//
// Request construction:
//   - method src.Method (GET when empty) and body src.Body;
//   - headers: src.Headers first, then the settings Headers, so explicit
//     per-task/queue/global settings win over headers captured with the
//     source (header names compare case-insensitively);
//   - User-Agent: src.UserAgent when set, else the settings UserAgent; it
//     replaces any User-Agent from the header maps;
//   - Referer: src.Referrer, else src.PageURL; when both are empty a
//     Referer from the header maps is kept;
//   - Range and Accept-Encoding (identity) are always set by Probe, so sizes
//     and offsets are byte-accurate. The client should not decompress
//     transparently (setting Accept-Encoding explicitly already disables
//     Go's automatic gzip handling).
//
// Response interpretation:
//   - 206 with a valid "Content-Range: bytes 0-N/TOTAL" means ranges are
//     supported and Size is TOTAL (-1 for "*"); a 206 with a malformed
//     Content-Range (or one not starting at 0) is closed and the request is
//     repeated without Range, because its body may not start at offset 0
//     and the downloader streams the probe body from the beginning;
//   - any other 2xx means the server ignored the range: Size comes from
//     Content-Length (-1 when absent) and RangeSupported reflects
//     "Accept-Ranges: bytes";
//   - 416 (typical for a 0-byte file) is retried once without Range;
//   - a Content-Encoding other than identity means the body is not the
//     raw file bytes: RangeSupported is false and Size is -1;
//   - a size of 0 never supports ranges (there is nothing to split).
//
// Non-2xx responses are closed and returned as *StatusError. Transport
// errors are returned wrapped and unclassified.
func Probe(ctx context.Context, client *http.Client, src domain.Source, s settings.Settings) (*ProbeResult, error) {
	if client == nil {
		return nil, errors.New("httpx: probe: nil client")
	}
	if strings.TrimSpace(src.CurrentURL) == "" {
		return nil, errors.New("httpx: probe: source has no current URL")
	}

	resp, err := doProbe(ctx, client, src, s, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		// "bytes=0-" is only unsatisfiable for an empty file. The 416 body is
		// not file content, so ask again without Range.
		_ = resp.Body.Close() // replaced by the retry; nothing useful left to read
		resp, err = doProbe(ctx, client, src, s, false)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode == http.StatusPartialContent && !isCompressed(resp.Header) {
		if _, ok := parseContentRange(resp.Header.Get("Content-Range")); !ok {
			// The body is a partial range we cannot place; it must not be
			// streamed as the start of the file.
			_ = resp.Body.Close() // replaced by the retry
			resp, err = doProbe(ctx, client, src, s, false)
			if err != nil {
				return nil, err
			}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close() // error response; only status and headers matter
		return nil, NewStatusError(resp)
	}
	return buildResult(resp, src), nil
}

func doProbe(ctx context.Context, client *http.Client, src domain.Source, s settings.Settings, withRange bool) (*http.Response, error) {
	req, err := newProbeRequest(ctx, src, s, withRange)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpx: probe: %w", err)
	}
	return resp, nil
}

func newProbeRequest(ctx context.Context, src domain.Source, s settings.Settings, withRange bool) (*http.Request, error) {
	req, err := newRequest(ctx, src, s, src.CurrentURL)
	if err != nil {
		return nil, fmt.Errorf("httpx: probe: %w", err)
	}
	if withRange {
		req.Header.Set("Range", "bytes=0-")
	}
	return req, nil
}

func buildResult(resp *http.Response, src domain.Source) *ProbeResult {
	size, rangeOK := sizeAndRange(resp)

	finalURL := resp.Request.URL
	out := src
	out.RedirectURL = ""
	if fu := finalURL.String(); fu != src.CurrentURL {
		out.RedirectURL = fu
	}

	name := DetectFilename(resp, finalURL)
	out.Fingerprint = domain.Fingerprint{
		Size:         size,
		ETag:         strongETag(resp.Header.Get("ETag")),
		LastModified: lastModified(resp.Header.Get("Last-Modified")),
		Filename:     name,
		ContentType:  mediaType(resp.Header.Get("Content-Type")),
	}

	return &ProbeResult{
		Response: resp,
		Source:   out,
		Resource: domain.Resource{
			Name:           name,
			Size:           size,
			RangeSupported: rangeOK,
			Files:          []domain.File{{Index: 0, Name: name, Size: size, Selected: true}},
		},
		Filename: name,
	}
}

// sizeAndRange derives the total size (-1 when unknown) and range support.
func sizeAndRange(resp *http.Response) (int64, bool) {
	if isCompressed(resp.Header) {
		return -1, false
	}
	if resp.StatusCode == http.StatusPartialContent {
		total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			return -1, false
		}
		return total, total != 0
	}
	size := resp.ContentLength
	if size < 0 {
		return -1, false
	}
	return size, size > 0 && acceptsByteRanges(resp.Header)
}

// parseContentRange validates "bytes 0-END/TOTAL" (TOTAL may be "*") and
// returns TOTAL, or -1 for "*". The start must be 0 because Probe asked for
// "bytes=0-", and END must be below TOTAL.
func parseContentRange(v string) (int64, bool) {
	start, _, total, ok := parseByteRange(v)
	if !ok || start != 0 {
		return 0, false
	}
	return total, true
}

// parseByteRange parses a single-range Content-Range value
// "bytes START-END/TOTAL" (END inclusive; TOTAL may be "*", returned as -1).
// It requires START <= END and, when TOTAL is known, END < TOTAL.
func parseByteRange(v string) (start, end, total int64, ok bool) {
	unit, rest, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, false
	}
	rng, totalStr, ok := strings.Cut(strings.TrimSpace(rest), "/")
	if !ok {
		return 0, 0, 0, false
	}
	startStr, endStr, ok := strings.Cut(rng, "-")
	if !ok {
		return 0, 0, 0, false
	}
	start, err1 := parseNonNeg(startStr)
	end, err2 := parseNonNeg(endStr)
	if err1 != nil || err2 != nil || end < start {
		return 0, 0, 0, false
	}
	if totalStr == "*" {
		return start, end, -1, true
	}
	total, err := parseNonNeg(totalStr)
	if err != nil || end >= total {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

func parseNonNeg(s string) (int64, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, errors.New("not a non-negative integer")
	}
	return strconv.ParseInt(s, 10, 64) //nolint:wrapcheck // callers only test for failure
}

func acceptsByteRanges(h http.Header) bool {
	for _, v := range h.Values("Accept-Ranges") {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "bytes") {
				return true
			}
		}
	}
	return false
}

func isCompressed(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		for tok := range strings.SplitSeq(v, ",") {
			if t := strings.TrimSpace(tok); t != "" && !strings.EqualFold(t, "identity") {
				return true
			}
		}
	}
	return false
}

// strongETag returns v when it is a strong entity tag ("..."), else "".
func strongETag(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "" // weak (W/"...") or malformed
	}
	return v
}

func lastModified(v string) *time.Time {
	if v == "" {
		return nil
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// mediaType returns the lower-case media type without parameters.
func mediaType(v string) string {
	if v == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(v); err == nil || mt != "" {
		return mt
	}
	mt, _, _ := strings.Cut(v, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}
