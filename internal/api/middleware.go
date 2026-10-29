package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/auth"
)

const (
	corsAllowMethods = "GET, POST, PUT, PATCH, DELETE"
	corsAllowHeaders = "Authorization, Content-Type, Last-Event-ID"
	corsMaxAge       = 10 * time.Minute
)

// buildHandler wraps the mux with the middleware chain. The outermost
// middleware runs first.
func (s *Server) buildHandler() (http.Handler, error) {
	allowed := make(map[string]struct{}, len(s.cfg.Server.AllowedOrigins))
	csrf := http.NewCrossOriginProtection()
	for _, origin := range s.cfg.Server.AllowedOrigins {
		if err := csrf.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("api: allowed origin: %w", err)
		}
		allowed[origin] = struct{}{}
	}
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusForbidden, "cross-origin request rejected")
	}))

	var h http.Handler = s.mux
	h = s.authMiddleware(h)
	h = bodyLimitMiddleware(h, MaxBodyBytes)
	h = csrf.Handler(h)
	h = corsMiddleware(h, allowed)
	h = s.recoverMiddleware(h)
	h = s.accessLogMiddleware(h)
	return h, nil
}

// writeProblem writes an RFC 9457 problem-details response using huma's
// configured error model, for errors raised outside huma operations.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	perr := huma.NewError(status, detail)
	ct := "application/json"
	if f, ok := perr.(huma.ContentTypeFilter); ok {
		ct = f.ContentType(ct)
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(perr.GetStatus())
	// The status line is already sent; an encode error means the client went
	// away and there is nothing left to report to it.
	_ = json.NewEncoder(w).Encode(perr)
}

// --- authentication -------------------------------------------------------

type authKey struct{}

// Authenticated reports whether the request carrying ctx presented a valid
// bearer token.
func Authenticated(ctx context.Context) bool {
	ok, _ := ctx.Value(authKey{}).(bool)
	return ok
}

type principalKey struct{}

// PrincipalOf returns who made the request carrying ctx; ok is false for an
// unauthenticated request (to a public operation).
func PrincipalOf(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

// readOnlyDenied are the paths a read-only token may not even read: the ones
// that hand out secrets or manage access.
func readOnlyDenied(path string) bool {
	return path == "/v1/cookies/export" || path == MCPPath || strings.HasPrefix(path, MCPPath+"/") ||
		path == "/v1/auth/tokens" || strings.HasPrefix(path, "/v1/auth/tokens/")
}

// extensionRoutes are the only operations a token with auth.ScopeExtension may
// call: handing downloads to the daemon and following them. Nothing that
// changes settings, manages access, exports secrets, installs the native host
// or deletes data is on the list. {x} matches one path segment.
var extensionRoutes = []struct{ method, pattern string }{
	{http.MethodGet, "/v1/auth/whoami"},
	{http.MethodGet, "/v1/system/info"},
	{http.MethodGet, "/v1/events"},
	{http.MethodGet, "/v1/limits"},
	{http.MethodPost, "/v1/limits/override"},
	{http.MethodGet, "/v1/queues"},
	{http.MethodGet, "/v1/queues/{id}"},
	{http.MethodGet, "/v1/categories"},
	{http.MethodGet, "/v1/categories/match"},
	{http.MethodGet, "/v1/tasks"},
	{http.MethodGet, "/v1/tasks/{id}"},
	{http.MethodGet, "/v1/tasks/{id}/segments"},
	{http.MethodGet, "/v1/tasks/{id}/refresh/history"},
	{http.MethodPost, "/v1/tasks"},
	{http.MethodPost, "/v1/tasks/batch"},
	{http.MethodPost, "/v1/tasks/expand"},
	{http.MethodPost, "/v1/tasks/torrent"},
	{http.MethodPost, "/v1/tasks/pause"},
	{http.MethodPost, "/v1/tasks/resume"},
	{http.MethodPost, "/v1/tasks/{id}/refresh"},
	{http.MethodPost, "/v1/tasks/{id}/refresh/await"},
	{http.MethodPost, "/v1/capture"},
	{http.MethodPost, "/v1/resolve"},
	{http.MethodPost, "/v1/resolve/batch"},
	{http.MethodPost, "/v1/browser/resolve"},
	{http.MethodPost, "/v1/browser/results/{id}/candidates/{index}/download"},
	{http.MethodPost, "/v1/cookies/import"},
}

// extensionAllowed reports whether a request with an extension-scoped token
// may go through.
func extensionAllowed(method, path string) bool {
	for _, r := range extensionRoutes {
		if r.method == method && pathMatches(r.pattern, path) {
			return true
		}
	}
	return false
}

// pathMatches compares a path with a pattern whose {x} segments match any one
// non-empty segment.
func pathMatches(pattern, path string) bool {
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(ps) != len(xs) {
		return false
	}
	for i, p := range ps {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			if xs[i] == "" {
				return false
			}
			continue
		}
		if p != xs[i] {
			return false
		}
	}
	return true
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, authed := s.authenticate(r.Context(), r.Header.Get("Authorization"))
		if !authed && s.requiresAuth(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dlm"`)
			writeProblem(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if authed && p.Scope == auth.ScopeRead {
			if (r.Method != http.MethodGet && r.Method != http.MethodHead) || readOnlyDenied(r.URL.Path) {
				writeCodedProblem(w, http.StatusForbidden, "auth.read_only", "this token is read-only")
				return
			}
		}
		if authed && p.Scope == auth.ScopeExtension && !extensionAllowed(r.Method, r.URL.Path) {
			writeCodedProblem(w, http.StatusForbidden, "auth.extension_scope", "a browser extension token may not do this")
			return
		}
		if authed && p.Scope == auth.ScopeExtension && !s.limitExtension(w, r, p) {
			return
		}
		ctx := context.WithValue(r.Context(), authKey{}, authed)
		if authed {
			ctx = context.WithValue(ctx, principalKey{}, p)
		}
		ctx = context.WithValue(ctx, localKey{}, isLoopbackRemote(r.RemoteAddr))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requiresAuth denies by default: everything under /v1 needs the token unless
// it routes to an operation added with RegisterPublic. Routes outside /v1
// (OpenAPI document, schemas, docs UI) are public.
func (s *Server) requiresAuth(r *http.Request) bool {
	_, pattern := s.mux.Handler(r)
	if s.isPublicPattern(pattern) {
		return false
	}
	path := r.URL.Path
	if pattern != "" {
		if _, p, ok := strings.Cut(pattern, " "); ok {
			path = p
		} else {
			path = pattern
		}
	}
	return path == "/v1" || strings.HasPrefix(path, PathPrefix) || path == MCPPath
}

// authenticate checks a presented bearer token: the primary one, or a named
// token.
func (s *Server) authenticate(ctx context.Context, header string) (auth.Principal, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return auth.Principal{}, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return auth.Principal{}, false
	}
	if primary := s.token.Load(); primary != nil && subtle.ConstantTimeCompare([]byte(token), *primary) == 1 {
		return auth.Principal{Name: "primary", Scope: auth.ScopeAdmin, Primary: true}, true
	}
	return s.tokens.Authenticate(ctx, token)
}

// writeCodedProblem is writeProblem with a stable code.
func writeCodedProblem(w http.ResponseWriter, status int, code, detail string) {
	p := newProblem(status, detail).withCode(code, nil)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// --- CORS -----------------------------------------------------------------

// corsMiddleware adds CORS headers only for the configured origins and
// answers their preflight requests. Requests without an Origin header (CLI,
// desktop app) pass through untouched.
func corsMiddleware(next http.Handler, allowed map[string]struct{}) http.Handler {
	maxAge := strconv.Itoa(int(corsMaxAge.Seconds()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		_, ok := allowed[origin]
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if !ok {
				writeProblem(w, http.StatusForbidden, "origin not allowed")
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", corsAllowMethods)
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", maxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if ok {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		next.ServeHTTP(w, r)
	})
}

// --- request size ---------------------------------------------------------

func bodyLimitMiddleware(next http.Handler, defaultLimit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := defaultLimit
		if r.Method == http.MethodPost && r.URL.Path == importPath {
			limit = MaxImportBodyBytes
		}
		if r.Method == http.MethodPost && r.URL.Path == torrentPath {
			limit = MaxTorrentBodyBytes
		}
		if r.ContentLength > limit {
			writeProblem(w, http.StatusRequestEntityTooLarge,
				"request body is too large, limit="+strconv.FormatInt(limit, 10)+" bytes")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// --- logging & recovery ---------------------------------------------------

// statusRecorder captures the response status and size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err //nolint:wrapcheck // must behave exactly like the wrapped writer
}

// Flush supports streaming responses (SSE).
func (r *statusRecorder) Flush() {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLogMiddleware logs one structured line per request. It logs only the
// path (never the query string, which may carry secrets) and no headers.
func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelDebug
		switch {
		case status >= http.StatusInternalServerError:
			level = slog.LevelWarn
		case status >= http.StatusBadRequest:
			level = slog.LevelInfo
		}
		s.log.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", r.RemoteAddr),
		)
	})
}

// recoverMiddleware turns a handler panic into a 500 problem response and
// logs the panic value with its stack.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w}
		}
		defer s.recoverPanic(rec, r)
		next.ServeHTTP(rec, r)
	})
}

// recoverPanic must be called directly by defer so that recover works.
func (s *Server) recoverPanic(rec *statusRecorder, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, compared as net/http does
		panic(v)
	}
	s.log.ErrorContext(r.Context(), "panic in http handler",
		"method", r.Method,
		"path", r.URL.Path,
		"panic", fmt.Sprint(v),
		"stack", string(debug.Stack()),
	)
	if rec.status != 0 {
		// The response has started; abort the connection so the client sees
		// a truncated response rather than a valid one.
		panic(http.ErrAbortHandler)
	}
	writeProblem(rec, http.StatusInternalServerError, "internal server error")
}
