package ytdlp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// call is what yt-dlp is started with for one task: the arguments that carry
// the task's source and settings, and the temporary files they refer to.
type call struct {
	common  []string
	cleanup func()
}

// args returns a copy of the common arguments.
func (c *call) args() []string { return append([]string(nil), c.common...) }

// prepare builds the arguments from the source and the effective settings.
// Every option is given as --name=value, so a value that starts with "-"
// cannot become an option of its own.
func (e *Engine) prepare(ctx context.Context, src domain.Source, s settings.Settings) (*call, error) {
	raw := strings.TrimSpace(src.CurrentURL)
	if strings.HasPrefix(raw, "-") {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("ytdlp: the URL must not start with a dash"))
	}
	c := &call{cleanup: func() {}}
	add := func(opt, value string) { c.common = append(c.common, "--"+opt+"="+value) }
	c.common = append(c.common, "--ignore-config", "--no-colors", "--no-warnings", "--no-cache-dir")

	if hint := strings.TrimSpace(src.MediaHint); hint != "" {
		add("format", hint)
	}
	if u, err := url.Parse(raw); err == nil {
		switch p := s.ProxyFor(u.Hostname()); p.Mode {
		case settings.ProxyCustom:
			add("proxy", p.URL)
		case settings.ProxyNone:
			add("proxy", "")
		case settings.ProxySystem:
		}
	}
	if !s.TLSVerify.OrElse(true) {
		c.common = append(c.common, "--no-check-certificates")
	}
	if ua := firstNonEmpty(src.UserAgent, s.UserAgent.OrElse("")); ua != "" {
		add("user-agent", ua)
	}
	if ref := firstNonEmpty(src.Referrer, src.PageURL); ref != "" {
		add("referer", ref)
	}
	for k, v := range src.Headers {
		add("add-header", k+":"+v)
	}
	for k, v := range s.Headers.OrElse(nil) {
		add("add-header", k+":"+v)
	}
	if n := s.Retry.MaxAttempts.OrElse(0); n > 0 {
		add("retries", strconv.Itoa(n))
		add("fragment-retries", strconv.Itoa(n))
	}
	if n := s.SpeedLimit.OrElse(0); n > 0 {
		add("limit-rate", strconv.FormatInt(n, 10))
	}
	path, cleanup, err := e.cookieFile(ctx, src)
	if err != nil {
		return nil, err
	}
	if path != "" {
		add("cookies", path)
		c.cleanup = cleanup
	}
	return c, nil
}

// cookieFile writes the stored cookies for the source's pages to a temporary
// Netscape file; path is empty when there are none.
func (e *Engine) cookieFile(ctx context.Context, src domain.Source) (path string, cleanup func(), err error) {
	if e.opts.cookies == nil {
		return "", nil, nil
	}
	var all []*domain.Cookie
	seen := map[string]bool{}
	for _, raw := range []string{src.CurrentURL, src.PageURL} {
		u, perr := url.Parse(strings.TrimSpace(raw))
		if raw == "" || perr != nil || u.Host == "" {
			continue
		}
		list, lerr := e.opts.cookies(ctx, u)
		if lerr != nil {
			return "", nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ytdlp: load the stored cookies: %w", lerr))
		}
		for _, c := range list {
			if k := c.Domain + "\x00" + c.Path + "\x00" + c.Name; !seen[k] {
				seen[k] = true
				all = append(all, c)
			}
		}
	}
	if len(all) == 0 {
		return "", nil, nil
	}
	path, cleanup, err = cookies.WriteTempNetscape(e.opts.tempDir, all)
	if err != nil {
		return "", nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	return path, cleanup, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
