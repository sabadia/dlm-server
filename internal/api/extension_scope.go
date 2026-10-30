package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// An extension token hands downloads to the daemon; it may not reach further
// than that. These checks run on every operation that creates a task for it
// and refuse what would let a leaked token act outside the download
// directory, run programs or reach the daemon's own machine.

// isExtension reports whether the caller holds an extension token.
func isExtension(ctx context.Context) bool {
	p, _ := PrincipalOf(ctx)
	return p.Scope == auth.ScopeExtension
}

func extensionForbidden(what string) error {
	return newProblem(http.StatusForbidden, "a browser extension token may not "+what).
		withCode("auth.extension_scope", map[string]string{"what": what})
}

// extensionSafeSettings returns the part of a task's settings that an
// extension token may set: limits, retries, the expected checksum, link
// refresh and seeding rules. Nothing that runs a program, picks a directory,
// names a proxy or relaxes TLS is in it.
func extensionSafeSettings(in settings.Settings) settings.Settings {
	var out settings.Settings
	out.MaxConnections = in.MaxConnections
	out.SpeedLimit = in.SpeedLimit
	out.Retry = in.Retry
	out.Checksum = in.Checksum
	out.Refresh = in.Refresh
	out.AutoCategorize = in.AutoCategorize
	out.Torrent.SeedKeep = in.Torrent.SeedKeep
	out.Torrent.SeedRatio = in.Torrent.SeedRatio
	out.Torrent.SeedTime = in.Torrent.SeedTime
	return out
}

// checkExtensionSettings refuses settings beyond extensionSafeSettings.
func checkExtensionSettings(in settings.Settings) error {
	if reflect.DeepEqual(in, extensionSafeSettings(in)) {
		return nil
	}
	return extensionForbidden("set these task settings (only limits, retries, checksum, refresh and seeding rules are allowed)")
}

// checkExtensionDir refuses a download directory outside the download
// directory and server.fs_roots.
func (s *Server) checkExtensionDir(ctx context.Context, dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil // nothing to check, or the service refuses it
	}
	clean := resolveExisting(filepath.Clean(dir))
	roots := append([]string(nil), s.cfg.Server.FSRoots...)
	if s.settings != nil {
		if d := s.settings.Defaults().DownloadDir.Value(); d != "" {
			roots = append(roots, d)
		}
		if g, err := s.settings.Global(ctx); err == nil {
			if d := g.DownloadDir.Value(); d != "" {
				roots = append(roots, d)
			}
		}
	}
	for _, r := range roots {
		if filepath.IsAbs(r) && within(resolveExisting(filepath.Clean(r)), clean) {
			return nil
		}
	}
	return extensionForbidden("download outside the download directory (server.fs_roots allows more)")
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves symbolic links in the longest existing prefix of
// path, so a link inside the download directory cannot lead out of it.
func resolveExisting(path string) string {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, rest)
		}
		if filepath.Dir(p) == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// checkExtensionURLs refuses links that point at the daemon's own machine:
// loopback, link-local (cloud metadata) and unspecified addresses, by literal
// or by name. Addresses of the local network stay allowed, a browser fetches
// from them too.
func checkExtensionURLs(ctx context.Context, urls ...string) error {
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue // not a network link (data:, magnet:) or invalid, refused later
		}
		if localHost(ctx, u.Hostname()) {
			return extensionForbidden("download from the machine the daemon runs on")
		}
	}
	return nil
}

func localHost(ctx context.Context, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return blockedIP(ip)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, h)
	if err != nil {
		return false // unresolvable: the download fails by itself
	}
	for _, a := range ips {
		if blockedIP(a.IP) {
			return true
		}
	}
	return false
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// checkExtensionSource checks every link of a source.
func checkExtensionSource(ctx context.Context, src domain.Source) error {
	urls := append([]string{src.CurrentURL, src.OriginalURL, src.RedirectURL}, src.Mirrors...)
	return checkExtensionURLs(ctx, urls...)
}

// guardExtension applies the checks above to a request that creates a task.
// It does nothing for other tokens.
func (s *Server) guardExtension(ctx context.Context, st settings.Settings, dir string, src domain.Source) error {
	if !isExtension(ctx) {
		return nil
	}
	if err := checkExtensionSettings(st); err != nil {
		return err
	}
	if err := s.checkExtensionDir(ctx, dir); err != nil {
		return err
	}
	return checkExtensionSource(ctx, src)
}
