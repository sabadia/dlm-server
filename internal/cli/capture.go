package cli

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

type captureFlags struct {
	request   string
	finalURL  string
	pageURL   string
	referrer  string
	filename  string
	mime      string
	size      string
	userAgent string
	headers   []string
	queue     string
	dir       string
	priority  string
	paused    bool
}

// request builds the capture request: the --request file first, then the
// flags and URL argument override it.
func (f *captureFlags) build(cmd *cobra.Command, args []string) (client.CaptureRequest, error) {
	var req client.CaptureRequest
	if f.request != "" {
		data, err := readInput(f.request, cmd.InOrStdin())
		if err != nil {
			return req, err
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return req, &usageError{err: fmt.Errorf("%s: not a capture request: %w", f.request, err)}
		}
	}
	if len(args) == 1 {
		req.URL = args[0]
	}
	fl := cmd.Flags()
	for _, o := range []struct {
		flag string
		dst  *string
		val  string
	}{
		{"final-url", &req.FinalURL, f.finalURL},
		{"page-url", &req.PageURL, f.pageURL},
		{"referrer", &req.Referrer, f.referrer},
		{"filename", &req.Filename, f.filename},
		{"mime", &req.MIME, f.mime},
		{"user-agent", &req.UserAgent, f.userAgent},
		{"queue", &req.QueueID, f.queue},
		{"dir", &req.Dir, f.dir},
		{"priority", &req.Priority, f.priority},
	} {
		if fl.Changed(o.flag) {
			*o.dst = o.val
		}
	}
	if fl.Changed("paused") {
		req.Paused = f.paused
	}
	if fl.Changed("size") {
		n, err := parseSize(f.size)
		if err != nil {
			return req, &usageError{err: err}
		}
		req.Size = n
	}
	if len(f.headers) > 0 {
		h, err := parseKeyValues("header", f.headers)
		if err != nil {
			return req, err
		}
		if req.Headers == nil {
			req.Headers = map[string]string{}
		}
		for k, v := range h {
			req.Headers[k] = v
		}
	}
	return req, nil
}

func (a *app) newCaptureCommand() *cobra.Command {
	var f captureFlags
	cmd := &cobra.Command{
		Use:   "capture [URL]",
		Short: "Create a task from a captured browser download",
		Long: "Create a task the way the browser extension does: with the page, referrer, headers, user agent, " +
			"cookies and media candidates the download was found with, which later let the link be refreshed.\n\n" +
			"Give the URL as an argument and describe the context with flags, or pass the extension's full JSON " +
			"request with --request FILE ('-' reads stdin; cookies and media candidates can only be set this way). " +
			"Flags override the file. The request's cookies are stored in the cookie store first.",
		Example: "  dlm capture https://cdn.example.com/f.zip --page-url https://example.com/get --referrer https://example.com/\n" +
			"  dlm capture -r capture.json --paused",
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{annotationOperationID: "capture-download"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := f.build(cmd, args)
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			res, err := c.CaptureDownload(cmd.Context(), req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format != output.FormatTable {
				return a.printer(cmd).Print(res, nil)
			}
			for _, e := range res.Cookies.Errors {
				writeErr(cmd.ErrOrStderr(), "cookie rejected: "+e)
			}
			return a.printTask(cmd, res.Task)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.request, "request", "r", "", "JSON capture request file ('-' for stdin)")
	fl.StringVar(&f.finalURL, "final-url", "", "URL after redirects; this is what is downloaded")
	fl.StringVar(&f.pageURL, "page-url", "", "web page the link was found on")
	fl.StringVar(&f.referrer, "referrer", "", "Referer to send (default: the page URL)")
	fl.StringVar(&f.filename, "filename", "", "file name chosen by the browser")
	fl.StringVar(&f.mime, "mime", "", "content type the browser saw")
	fl.StringVar(&f.size, "size", "", "size the browser saw: "+sizeHelp)
	fl.StringVar(&f.userAgent, "user-agent", "", "user agent the browser used")
	fl.StringArrayVar(&f.headers, "header", nil, "request header Name=Value to repeat (repeatable; Cookie and Host are ignored)")
	fl.StringVar(&f.queue, "queue", "", "queue ID or name (default: Main)")
	fl.StringVar(&f.dir, "dir", "", "absolute download directory")
	fl.StringVar(&f.priority, "priority", "", "priority: low, normal or high")
	fl.BoolVar(&f.paused, "paused", false, "create the task paused")
	return cmd
}
