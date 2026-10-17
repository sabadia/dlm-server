package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

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
}

func (c CaptureCookie) toDomain() *domain.Cookie {
	return &domain.Cookie{
		Domain: c.Domain, Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly,
		SameSite: c.SameSite, HostOnly: c.HostOnly, ExpiresAt: c.ExpiresAt,
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

func (b *CaptureBody) toInput() service.CaptureInput {
	in := service.CaptureInput{
		URL: b.URL, FinalURL: b.FinalURL, PageURL: b.PageURL, Referrer: b.Referrer,
		Filename: b.Filename, MIME: b.MIME, Size: b.Size, UserAgent: b.UserAgent, Headers: b.Headers,
		QueueID: b.QueueID, Dir: b.Dir, Settings: b.Settings, Priority: b.Priority, Labels: b.Labels, Paused: b.Paused,
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
			"The extension's origin must be listed in the server's allowed origins.",
		Tags:          []string{tagCapture},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.captureDownload)
}

func (s *Server) captureDownload(ctx context.Context, in *struct{ Body CaptureBody }) (*struct{ Body CaptureOutput }, error) {
	if s.capture == nil {
		return nil, errServicesUnavailable()
	}
	res, err := s.capture.Capture(ctx, in.Body.toInput())
	if err != nil {
		return nil, s.fail(ctx, "capture-download", err, locBodySettings)
	}
	return &struct{ Body CaptureOutput }{Body: CaptureOutput{
		Task: res.Task, Refreshed: res.Refreshed,
		Cookies: ImportCookiesResult{
			Imported: res.Cookies.Imported, Skipped: res.Cookies.Skipped, Errors: res.Cookies.Errors,
		},
	}}, nil
}
