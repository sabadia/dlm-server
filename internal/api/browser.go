package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

const tagBrowser = "browser"

// ResolvePageBody is the request of resolve-page.
type ResolvePageBody struct {
	URL         string `json:"url" minLength:"1" doc:"Absolute http(s) URL of the page to open."`
	WaitSeconds int    `json:"waitSeconds,omitempty" minimum:"1" maximum:"120" doc:"How long the page may keep producing downloads before it counts as settled (default 20)."`
}

// DownloadCandidateBody is the request of download-candidate.
type DownloadCandidateBody struct {
	QueueID  string            `json:"queueId,omitempty" doc:"Queue ID or name (default: the Main queue)."`
	Name     string            `json:"name,omitempty" doc:"File name without directories (default: the one the page announced)."`
	Dir      string            `json:"dir,omitempty" doc:"Absolute download directory."`
	Settings settings.Settings `json:"settings,omitzero" doc:"Task-level settings overrides."`
	Priority domain.Priority   `json:"priority,omitempty" enum:"low,normal,high"`
	Labels   map[string]string `json:"labels,omitempty"`
	Paused   bool              `json:"paused,omitempty" doc:"Create the task paused instead of queued."`
}

// BrowserProfiles is the response of list-browser-profiles.
type BrowserProfiles struct {
	Profiles []string `json:"profiles" doc:"Site groups that have a browser profile."`
}

func (s *Server) registerBrowser() {
	Register(s, huma.Operation{
		OperationID: "resolve-page",
		Method:      http.MethodPost,
		Path:        "/v1/browser/resolve",
		Summary:     "Open a page in the headless browser and list its downloads",
		Description: "Opens the page in the internal headless browser, with the stored cookies of its site, and returns the downloads it led to: " +
			"HLS and DASH manifests, video and audio files, file downloads and JS-made blob downloads (which the browser saves and the daemon keeps). " +
			"The cookies the page set are stored. The result and its files are kept for a while: pass its id and a candidate index to download-candidate.",
		Tags:   []string{tagBrowser},
		Errors: []int{http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, s.resolvePage)

	Register(s, huma.Operation{
		OperationID:   "download-candidate",
		Method:        http.MethodPost,
		Path:          "/v1/browser/results/{id}/candidates/{index}/download",
		Summary:       "Download a candidate found on a page",
		Description:   "Creates a task for a candidate of a resolve-page result. Files the browser downloaded itself are imported; other candidates are downloaded from their URL. HLS playlists (.m3u8) are downloaded by the HLS engine; DASH manifests (.mpd) by the DASH engine; a stream whose URL has no .m3u8/.mpd extension is refused (422).",
		Tags:          []string{tagBrowser, tagTasks},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.downloadCandidate)

	Register(s, huma.Operation{
		OperationID: "get-browser-status",
		Method:      http.MethodGet,
		Path:        "/v1/browser",
		Summary:     "Get the state of the headless browser",
		Description: "The browser in use (absent until a page was opened) and the running browsers, one per site group.",
		Tags:        []string{tagBrowser},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.getBrowserStatus)

	Register(s, huma.Operation{
		OperationID: "list-browser-profiles",
		Method:      http.MethodGet,
		Path:        "/v1/browser/profiles",
		Summary:     "List browser profiles",
		Description: "Each site group has its own profile, which keeps the logins of the pages opened there.",
		Tags:        []string{tagBrowser},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listBrowserProfiles)

	Register(s, huma.Operation{
		OperationID:   "delete-browser-profile",
		Method:        http.MethodDelete,
		Path:          "/v1/browser/profiles/{group}",
		Summary:       "Delete a browser profile",
		Description:   "Removes the profile (cookies and storage) of a site group. 409 while its browser is running.",
		Tags:          []string{tagBrowser},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, s.deleteBrowserProfile)
}

func (s *Server) resolvePage(ctx context.Context, in *struct{ Body ResolvePageBody }) (*struct{ Body *browser.PageResult }, error) {
	if s.browser == nil {
		return nil, errServicesUnavailable()
	}
	res, err := s.browser.ResolvePage(ctx, service.ResolvePageInput{URL: in.Body.URL, Wait: time.Duration(in.Body.WaitSeconds) * time.Second})
	if err != nil {
		return nil, s.fail(ctx, "resolve-page", err, locBody)
	}
	return &struct{ Body *browser.PageResult }{Body: res}, nil
}

func (s *Server) downloadCandidate(ctx context.Context, in *struct {
	ID    string `path:"id" doc:"resolve-page result ID."`
	Index int    `path:"index" minimum:"0" doc:"Candidate index within the result."`
	Body  DownloadCandidateBody
},
) (*taskOutput, error) {
	if s.browser == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.browser.Download(ctx, service.BrowserDownloadInput{
		ResultID: in.ID, Index: in.Index, QueueID: in.Body.QueueID, Name: in.Body.Name, Dir: in.Body.Dir,
		Settings: in.Body.Settings, Priority: in.Body.Priority, Labels: in.Body.Labels, Paused: in.Body.Paused,
	})
	if err != nil {
		return nil, s.fail(ctx, "download-candidate", err, locBodySettings)
	}
	return &taskOutput{Body: t}, nil
}

func (s *Server) getBrowserStatus(_ context.Context, _ *struct{}) (*struct{ Body browser.Status }, error) {
	if s.browser == nil {
		return nil, errServicesUnavailable()
	}
	return &struct{ Body browser.Status }{Body: s.browser.Status()}, nil
}

func (s *Server) listBrowserProfiles(ctx context.Context, _ *struct{}) (*struct{ Body BrowserProfiles }, error) {
	if s.browser == nil {
		return nil, errServicesUnavailable()
	}
	p, err := s.browser.Profiles()
	if err != nil {
		return nil, s.fail(ctx, "list-browser-profiles", err, locBody)
	}
	return &struct{ Body BrowserProfiles }{Body: BrowserProfiles{Profiles: p}}, nil
}

func (s *Server) deleteBrowserProfile(ctx context.Context, in *struct {
	Group string `path:"group" doc:"Site group, as listed by list-browser-profiles."`
},
) (*struct{}, error) {
	if s.browser == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.browser.DeleteProfile(in.Group); err != nil {
		return nil, s.fail(ctx, "delete-browser-profile", err, locBody)
	}
	return &struct{}{}, nil
}
