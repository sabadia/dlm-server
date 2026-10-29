package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds a single request unless overridden by WithTimeout.
	DefaultTimeout = 30 * time.Second
	// InstallTimeout bounds installing a plugin, which may clone a repository.
	InstallTimeout = 3 * time.Minute
	// DefaultUserAgent is sent unless overridden by WithUserAgent.
	DefaultUserAgent = "dlm-cli"

	// maxErrorBody bounds how much of an error response is read.
	maxErrorBody = 1 << 20
)

// ErrDaemonUnreachable is matched (errors.Is) by errors returned when no
// daemon accepts connections at the client's base URL.
var ErrDaemonUnreachable = errors.New("dlm daemon is not running")

// Client calls the dlm REST API. It is safe for concurrent use.
type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	userAgent string
	timeout   time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client (default: a new client
// with no global timeout; per-request timeouts come from WithTimeout).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.http = hc
		}
	}
}

// WithUserAgent sets the User-Agent header, e.g. "dlm-cli/1.2.3".
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// WithTimeout bounds each request. Zero or negative disables the timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// New creates a client for the API at baseURL (e.g. "http://127.0.0.1:17171").
// token is sent as a bearer token; an empty token sends no Authorization
// header, which is enough for public operations such as Health.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("client: invalid base URL %q: %w", baseURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("client: invalid base URL %q: want http(s)://host:port", baseURL)
	}
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		http:      &http.Client{},
		userAgent: DefaultUserAgent,
		timeout:   DefaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL returns the API base URL the client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// HasToken reports whether the client sends a bearer token.
func (c *Client) HasToken() bool { return c.token != "" }

// do sends a request with an optional JSON body (in) and decodes a JSON
// success response into out (if non-nil). Error responses become *APIError.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doAs(ctx, method, path, "application/json", in, out)
}

// doBytes sends data as the request body with the given media type and decodes
// the JSON answer into out.
func (c *Client) doBytes(ctx context.Context, method, path, contentType string, data []byte, out any) error {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := c.newRequest(ctx, method, path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, application/problem+json")
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("client: decode %s %s response: %w", method, path, err)
	}
	return nil
}

// doAs is do with the media type of the request body (a JSON-based type
// such as application/merge-patch+json).
func (c *Client) doAs(ctx context.Context, method, path, contentType string, in, out any) error {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("client: encode %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, application/problem+json")
	if in != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(method, path, err)
	}
	defer func() {
		// Drain so the connection can be reused; errors here are irrelevant
		// once the response has been handled.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("client: decode %s %s response: %w", method, path, err)
	}
	return nil
}

// newRequest builds a request to path carrying the User-Agent and, if set,
// the bearer token.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("client: build %s %s request: %w", method, path, err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// transportError wraps an http.Client.Do error, mapping connection failures
// to ErrDaemonUnreachable.
func (c *Client) transportError(method, path string, err error) error {
	if isDialFailure(err) {
		return fmt.Errorf("%w at %s (start it with `dlm serve`): %w", ErrDaemonUnreachable, c.baseURL, err)
	}
	return fmt.Errorf("client: %s %s: %w", method, path, err)
}

// isDialFailure reports whether err is a failure to establish the TCP
// connection (as opposed to a timeout or a failure after connecting). It
// relies on net.OpError rather than errno values, which differ per OS.
func isDialFailure(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		return false
	}
	return !opErr.Timeout()
}

func decodeError(resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")) // unparsable means "not JSON"
	if readErr == nil && (mediaType == "application/problem+json" || mediaType == "application/json") {
		// A body that is not a problem document still yields a usable error
		// from the status code below.
		_ = json.Unmarshal(data, apiErr)
		apiErr.Status = resp.StatusCode
	}
	if apiErr.Title == "" {
		apiErr.Title = http.StatusText(resp.StatusCode)
	}
	return apiErr
}
