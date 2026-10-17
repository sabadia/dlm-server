package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

const tagCookies = "cookies"

// CookieInfo describes a stored cookie. The value is a secret and is never
// listed; export-cookies returns it on explicit request.
type CookieInfo struct {
	ID           string     `json:"id" doc:"Cookie ID."`
	Domain       string     `json:"domain" doc:"Domain the cookie was set for, lower-case without a leading dot."`
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	PartitionKey string     `json:"partitionKey,omitempty" doc:"Top-level site of a partitioned (CHIPS) cookie; empty for ordinary cookies."`
	Secure       bool       `json:"secure"`
	HTTPOnly     bool       `json:"httpOnly"`
	SameSite     string     `json:"sameSite,omitempty" enum:"lax,strict,none"`
	HostOnly     bool       `json:"hostOnly" doc:"True when only the exact host receives the cookie, not its subdomains."`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty" doc:"Absent for session cookies."`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

func cookieInfo(c *domain.Cookie) CookieInfo {
	return CookieInfo{
		ID: c.ID, Domain: c.Domain, Name: c.Name, Path: c.Path, PartitionKey: c.PartitionKey,
		Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite, HostOnly: c.HostOnly,
		ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// CookieList is the response of list-cookies.
type CookieList struct {
	Cookies []CookieInfo `json:"cookies" doc:"Stored cookies ordered by domain, path and name; values are not included."`
}

// DeleteCookiesResult is the response of delete-cookies.
type DeleteCookiesResult struct {
	Deleted int `json:"deleted" doc:"Number of cookies removed."`
}

// ImportCookiesBody is the request of import-cookies.
type ImportCookiesBody struct {
	Format string `json:"format,omitempty" enum:"json,netscape" doc:"Format of data; detected from the content when absent."`
	Data   string `json:"data" minLength:"1" doc:"The cookie file content: a JSON array of cookies (also the export of browser cookie extensions) or a Netscape cookies.txt."`
}

// ImportCookiesResult is the response of import-cookies.
type ImportCookiesResult struct {
	Imported int      `json:"imported" doc:"Cookies stored (new or replacing an existing one)."`
	Skipped  int      `json:"skipped" doc:"Valid cookies that were already expired."`
	Errors   []string `json:"errors" doc:"One entry per rejected cookie, naming its position and the reason."`
}

// ExportCookiesResult is the response of export-cookies.
type ExportCookiesResult struct {
	Format string `json:"format" enum:"json,netscape"`
	Count  int    `json:"count" doc:"Cookies in data."`
	Data   string `json:"data" doc:"The cookie file content. It contains cookie values: treat it as a secret."`
}

type cookieFilterInput struct {
	Domain string `query:"domain" doc:"Only cookies of exactly this domain (case-insensitive, no subdomains)."`
	Name   string `query:"name" doc:"Only cookies with this name."`
}

func (s *Server) registerCookies() {
	Register(s, huma.Operation{
		OperationID: "list-cookies",
		Method:      http.MethodGet,
		Path:        "/v1/cookies",
		Summary:     "List stored cookies",
		Description: "Lists the cookie store without values. Cookie values are encrypted at rest and only returned by export-cookies.",
		Tags:        []string{tagCookies},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listCookies)

	Register(s, huma.Operation{
		OperationID: "delete-cookies",
		Method:      http.MethodDelete,
		Path:        "/v1/cookies",
		Summary:     "Delete the cookies of a domain",
		Description: "Deletes every cookie stored for exactly this domain (subdomains are separate domains).",
		Tags:        []string{tagCookies},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.deleteCookies)

	Register(s, huma.Operation{
		OperationID:   "delete-cookie",
		Method:        http.MethodDelete,
		Path:          "/v1/cookies/{id}",
		Summary:       "Delete a cookie",
		Tags:          []string{tagCookies},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.deleteCookie)

	Register(s, huma.Operation{
		OperationID: "import-cookies",
		Method:      http.MethodPost,
		Path:        "/v1/cookies/import",
		Summary:     "Import cookies",
		Description: "Stores the cookies of a JSON or Netscape cookies.txt file. A cookie with the same domain, name, path and partition key is replaced. " +
			"Rejected cookies are listed in the result; the request fails only when the document cannot be read.",
		Tags:   []string{tagCookies},
		Errors: []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.importCookies)

	Register(s, huma.Operation{
		OperationID: "export-cookies",
		Method:      http.MethodGet,
		Path:        "/v1/cookies/export",
		Summary:     "Export cookies",
		Description: "Returns the unexpired cookies, values included, as a JSON or Netscape cookies.txt file.",
		Tags:        []string{tagCookies},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.exportCookies)
}

func (s *Server) listCookies(ctx context.Context, in *cookieFilterInput) (*struct{ Body CookieList }, error) {
	if s.cookies == nil {
		return nil, errServicesUnavailable()
	}
	cs, err := s.cookies.List(ctx, service.CookieFilter{Domain: in.Domain, Name: in.Name})
	if err != nil {
		return nil, s.fail(ctx, "list-cookies", err, locBody)
	}
	out := CookieList{Cookies: make([]CookieInfo, len(cs))}
	for i, c := range cs {
		out.Cookies[i] = cookieInfo(c)
	}
	return &struct{ Body CookieList }{Body: out}, nil
}

func (s *Server) deleteCookies(ctx context.Context, in *struct {
	Domain string `query:"domain" required:"true" minLength:"1" doc:"Domain whose cookies are deleted."`
},
) (*struct{ Body DeleteCookiesResult }, error) {
	if s.cookies == nil {
		return nil, errServicesUnavailable()
	}
	n, err := s.cookies.DeleteDomain(ctx, in.Domain)
	if err != nil {
		return nil, s.fail(ctx, "delete-cookies", err, locBody)
	}
	return &struct{ Body DeleteCookiesResult }{Body: DeleteCookiesResult{Deleted: n}}, nil
}

func (s *Server) deleteCookie(ctx context.Context, in *struct {
	ID string `path:"id" doc:"Cookie ID."`
},
) (*struct{}, error) {
	if s.cookies == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.cookies.Delete(ctx, in.ID); err != nil {
		return nil, s.fail(ctx, "delete-cookie", err, locBody)
	}
	return &struct{}{}, nil
}

func (s *Server) importCookies(ctx context.Context, in *struct{ Body ImportCookiesBody }) (*struct{ Body ImportCookiesResult }, error) {
	if s.cookies == nil {
		return nil, errServicesUnavailable()
	}
	res, err := s.cookies.Import(ctx, in.Body.Format, in.Body.Data)
	if err != nil {
		return nil, s.fail(ctx, "import-cookies", err, locBody)
	}
	return &struct{ Body ImportCookiesResult }{Body: ImportCookiesResult{
		Imported: res.Imported, Skipped: res.Skipped, Errors: res.Errors,
	}}, nil
}

func (s *Server) exportCookies(ctx context.Context, in *struct {
	Domain string `query:"domain" doc:"Only cookies of exactly this domain (case-insensitive, no subdomains)."`
	Name   string `query:"name" doc:"Only cookies with this name."`
	Format string `query:"format" enum:"json,netscape" default:"json" doc:"File format."`
},
) (*struct{ Body ExportCookiesResult }, error) {
	if s.cookies == nil {
		return nil, errServicesUnavailable()
	}
	exp, err := s.cookies.Export(ctx, in.Format, service.CookieFilter{Domain: in.Domain, Name: in.Name})
	if err != nil {
		return nil, s.fail(ctx, "export-cookies", err, locBody)
	}
	return &struct{ Body ExportCookiesResult }{Body: ExportCookiesResult{
		Format: string(exp.Format), Count: exp.Count, Data: exp.Data,
	}}, nil
}
