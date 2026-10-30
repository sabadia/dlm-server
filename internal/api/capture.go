package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

const tagCapture = "capture"

// CaptureCookie is a cookie the browser held for the captured download.
type CaptureCookie struct {
	Domain    string     `json:"domain" minLength:"1" doc:"Cookie domain; a leading dot is ignored."`
	Name      string     `json:"name" minLength:"1"`
	Value     string     `json:"value"`
	Path      string     `json:"path,omitempty" doc:"Default /."`
	Secure    bool       `json:"secure,omitempty"`
	HTTPOnly  bool       `json:"httpOnly,omitempty"`
	SameSite  string     `json:"sameSite,omitempty" enum:"lax,strict,none"`
	HostOnly  bool       `json:"hostOnly,omitempty" doc:"True when only the exact host receives the cookie (no Domain attribute)."`
	ExpiresAt *time.Time `json:"expiresAt,omitempty" doc:"Absent for session cookies."`
	// PartitionKey is the browser's partition key of a partitioned (CHIPS) cookie.
	PartitionKey string `json:"partitionKey,omitempty" doc:"Partition key of a partitioned cookie (CHIPS), as the browser reports it. Kept as it is in the cookie store; dropped for a task's own cookies (cookieScope task), which are sent like ordinary ones."`
}

func (c CaptureCookie) toDomain() *domain.Cookie {
	return &domain.Cookie{
		Domain: c.Domain, Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly,
		SameSite: c.SameSite, HostOnly: c.HostOnly, ExpiresAt: c.ExpiresAt, PartitionKey: c.PartitionKey,
	}
}

// MediaCandidateBody is a media resource found on the page.
type MediaCandidateBody struct {
	URL     string `json:"url" minLength:"1" doc:"Absolute URL of the resource (manifest or file)."`
	Kind    string `json:"kind,omitempty" enum:"hls,dash,video,audio,file" doc:"Resource kind; unset counts as file."`
	MIME    string `json:"mime,omitempty"`
	Size    int64  `json:"size,omitempty" doc:"Bytes, when known."`
	Variant string `json:"variant,omitempty" doc:"Stream variant such as 1080p; becomes the task's mediaHint."`
}

// CaptureChecksum is the hash the file is expected to have.
type CaptureChecksum struct {
	Algorithm settings.ChecksumAlgorithm `json:"algorithm" enum:"md5,sha1,sha256,blake3"`
	Expected  string                     `json:"expected" minLength:"1" doc:"The hash, hex encoded."`
}

// CaptureBody is the request of capture-download: a download the browser
// extension intercepted, with the context needed to fetch it and to find the
// link again after it expires.
type CaptureBody struct {
	URL      string `json:"url,omitempty" doc:"Link the user clicked; becomes the task's originalUrl. May be omitted when mediaCandidates is set: the best candidate is then downloaded (hls, dash, video, audio, file; larger first)."`
	FinalURL string `json:"finalUrl,omitempty" doc:"URL after the browser followed redirects; this is what is downloaded."`
	PageURL  string `json:"pageUrl,omitempty" doc:"Web page the link was found on."`
	Referrer string `json:"referrer,omitempty" doc:"Referer the browser sent (default pageUrl)."`
	Filename string `json:"filename,omitempty" doc:"File name chosen by the browser (directories are dropped)."`
	MIME     string `json:"mime,omitempty" doc:"Content type the browser saw."`
	Size     int64  `json:"size,omitempty" doc:"Size in bytes the browser saw."`

	Cookies         []CaptureCookie      `json:"cookies,omitempty" doc:"Cookies for the download; stored in the cookie store before the task is queued."`
	UserAgent       string               `json:"userAgent,omitempty" doc:"User agent the browser used."`
	Headers         map[string]string    `json:"headers,omitempty" doc:"Request headers to repeat; Cookie and Host are ignored."`
	MediaCandidates []MediaCandidateBody `json:"mediaCandidates,omitempty" doc:"Media found on the page. Selects the download when url is absent; when url equals a candidate its variant, mime and size fill in what the request omits."`

	QueueID  string            `json:"queueId,omitempty" doc:"Queue ID or name (default: the Main queue)."`
	Dir      string            `json:"dir,omitempty" doc:"Absolute download directory."`
	Settings settings.Settings `json:"settings,omitzero" doc:"Task-level settings overrides."`
	Priority domain.Priority   `json:"priority,omitempty" enum:"low,normal,high"`
	Labels   map[string]string `json:"labels,omitempty"`
	Paused   bool              `json:"paused,omitempty" doc:"Create the task paused instead of queued."`
	// AllowDuplicate lets the capture add a task that repeats another one.
	AllowDuplicate bool `json:"allowDuplicate,omitempty" doc:"Add the task even when another task has the same link; otherwise it is refused with 409."`

	Name         string              `json:"name,omitempty" doc:"File name for the task; wins over filename."`
	ResolverKind domain.ResolverKind `json:"resolverKind,omitempty" enum:"direct,ytdlp,browser" doc:"How the link is turned into a download: direct (a file, the default), ytdlp (a video or audio page yt-dlp can extract) or browser (a page that needs the headless browser)."`
	Checksum     *CaptureChecksum    `json:"checksum,omitempty" doc:"The hash the file should have; the task fails and re-downloads on a mismatch (task setting checksum)."`
	Mirrors      []string            `json:"mirrors,omitempty" maxItems:"16" doc:"Other http(s) links to the same file."`
	Method       string              `json:"method,omitempty" doc:"HTTP method; default GET. For a form that answers with a file."`
	Body         []byte              `json:"body,omitempty" doc:"Request body for a non-GET method, base64; at most 256 KiB."`
	CookieScope  string              `json:"cookieScope,omitempty" enum:"task,store" doc:"task keeps the cookies for this download only (they are not in the cookie store and are removed when the task completes or is deleted, or after 7 days); store adds them to the cookie store like cookies/import. Default: task for a browser-extension token, store for any other."`
}

// CaptureOutput is the response of capture-download.
type CaptureOutput struct {
	Task *domain.Task `json:"task" doc:"The created task, or the existing task that received the captured link."`
	// Refreshed is true when no task was created: the capture matched a task waiting for a new link
	// (refresh strategy extension-capture) and replaced its link; the task is queued again.
	Refreshed bool `json:"refreshed" doc:"True when the capture matched a task that was waiting for a new link and gave it the link instead of creating a task."`
	// Cookies describes what happened to the request's cookies.
	Cookies ImportCookiesResult `json:"cookies" doc:"Result of storing the request's cookies."`
}

func (b *CaptureBody) toInput(extension bool) service.CaptureInput {
	taskCookies := extension
	switch b.CookieScope {
	case "task":
		taskCookies = true
	case "store":
		taskCookies = false
	}
	in := service.CaptureInput{
		Name: b.Name, ResolverKind: b.ResolverKind, Mirrors: b.Mirrors, Method: b.Method, Body: b.Body, TaskCookies: taskCookies,
		URL: b.URL, FinalURL: b.FinalURL, PageURL: b.PageURL, Referrer: b.Referrer,
		Filename: b.Filename, MIME: b.MIME, Size: b.Size, UserAgent: b.UserAgent, Headers: b.Headers,
		QueueID: b.QueueID, Dir: b.Dir, Settings: b.Settings, Priority: b.Priority, Labels: b.Labels, Paused: b.Paused, AllowDuplicate: b.AllowDuplicate,
	}
	if b.Checksum != nil {
		in.Checksum = &settings.Checksum{Algorithm: b.Checksum.Algorithm, Expected: b.Checksum.Expected}
	}
	for _, c := range b.Cookies {
		in.Cookies = append(in.Cookies, c.toDomain())
	}
	for _, m := range b.MediaCandidates {
		in.MediaCandidates = append(in.MediaCandidates, service.MediaCandidate{
			URL: m.URL, Kind: service.MediaKind(m.Kind), MIME: m.MIME, Size: m.Size, Variant: m.Variant,
		})
	}
	return in
}

func (s *Server) registerCapture() {
	Register(s, huma.Operation{
		OperationID: "capture-download",
		Method:      http.MethodPost,
		Path:        "/v1/capture",
		Summary:     "Capture a download from the browser extension",
		Description: "Creates a task from a download the browser extension intercepted, keeping the context needed to refresh the link " +
			"later (original and final URL, page, referrer, headers, user agent, cookie domains, media hint). " +
			"The request's cookies are stored before the task is queued, so the first request already carries them. " +
			"The cookies of a browser-extension token are kept for this download only (cookieScope).",
		Tags:          []string{tagCapture},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.captureDownload)
}

func (s *Server) captureDownload(ctx context.Context, in *struct{ Body CaptureBody }) (*struct{ Body CaptureOutput }, error) {
	if s.capture == nil {
		return nil, errServicesUnavailable()
	}
	p, _ := PrincipalOf(ctx)
	if p.Scope == auth.ScopeExtension {
		if err := checkExtensionCapture(&in.Body); err != nil {
			return nil, err
		}
		if err := checkExtensionSettings(in.Body.Settings); err != nil {
			return nil, err
		}
		if err := s.checkExtensionDir(ctx, in.Body.Dir); err != nil {
			return nil, err
		}
		urls := append([]string{in.Body.URL, in.Body.FinalURL}, in.Body.Mirrors...)
		for _, c := range in.Body.MediaCandidates {
			urls = append(urls, c.URL)
		}
		if err := checkExtensionURLs(ctx, urls...); err != nil {
			return nil, err
		}
	}
	res, err := s.capture.Capture(ctx, in.Body.toInput(p.Scope == auth.ScopeExtension))
	if err != nil {
		return nil, s.fail(ctx, "capture-download", err, locBodySettings)
	}
	return &struct{ Body CaptureOutput }{Body: CaptureOutput{
		Task: redactTaskFor(ctx, res.Task), Refreshed: res.Refreshed,
		Cookies: ImportCookiesResult{
			Imported: res.Cookies.Imported, Skipped: res.Cookies.Skipped, Errors: res.Cookies.Errors,
		},
	}}, nil
}
