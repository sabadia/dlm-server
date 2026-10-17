package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

// watchSummaryMax bounds the SUMMARY column for event types without a
// dedicated summary.
const watchSummaryMax = 80

func (a *app) newWatchCommand() *cobra.Command {
	var opts client.EventsOptions
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream live events from the daemon",
		Long: "Print events (task progress and status changes, queue and settings updates) as they happen.\n\n" +
			"Table output prints one line per event (TIME TYPE TASK QUEUE SUMMARY); json prints one JSON object per " +
			"line (NDJSON); yaml prints one document per event. The stream reconnects automatically and resumes " +
			"after the last received event. When events may have been missed (daemon restart, slow reader) a notice " +
			"is printed to stderr. Stop with Ctrl-C.",
		Example: "  dlm watch\n  dlm watch --type task.progress --type task.status --task <id>\n  dlm watch -o json",
		Args:    cobra.NoArgs,
		Annotations: map[string]string{
			annotationOperationID: "stream-events",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			opts.Reconnect = true
			opts.OnRetry = func(err error, delay time.Duration) {
				writeErr(stderr, fmt.Sprintf("note: event stream interrupted (%v); reconnecting in %s", err, delay.Round(time.Millisecond)))
			}
			w := &watchWriter{out: cmd.OutOrStdout(), errOut: stderr, format: a.format}
			if err := w.header(); err != nil {
				return err
			}
			for ev, err := range c.Events(cmd.Context(), opts) {
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if err := w.event(ev); err != nil {
					return err
				}
			}
			return nil // ctx cancelled (Ctrl-C)
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&opts.Types, "type", nil, "only show events of this type (repeatable or comma-separated), e.g. task.progress")
	f.StringVar(&opts.TaskID, "task", "", "only show events of this task ID")
	f.StringVar(&opts.QueueID, "queue", "", "only show events of this queue ID")
	return cmd
}

// watchWriter renders streamed events in the selected output format.
type watchWriter struct {
	out, errOut io.Writer
	format      output.Format
}

const watchRowFormat = "%-8s  %-16s  %-36s  %-36s  %s\n"

func (w *watchWriter) header() error {
	if w.format != output.FormatTable {
		return nil
	}
	if _, err := fmt.Fprintf(w.out, watchRowFormat, "TIME", "TYPE", "TASK", "QUEUE", "SUMMARY"); err != nil {
		return fmt.Errorf("cli: write: %w", err)
	}
	return nil
}

func (w *watchWriter) event(ev client.Event) error {
	if ev.Type == client.EventTypeReset {
		writeErr(w.errOut, "note: event stream reset ("+ev.ResetReason()+"): some events may have been missed; refetch state if you mirror it")
		if w.format == output.FormatTable {
			return nil
		}
	}
	var err error
	switch w.format {
	case output.FormatJSON:
		var data []byte
		if data, err = json.Marshal(ev); err == nil {
			_, err = w.out.Write(append(data, '\n'))
		}
	case output.FormatYAML:
		if _, err = io.WriteString(w.out, "---\n"); err == nil {
			err = output.New(w.out, output.FormatYAML).Print(ev, nil)
		}
	default:
		_, err = fmt.Fprintf(w.out, watchRowFormat,
			ev.Time.Local().Format(time.TimeOnly), ev.Type, orDash(ev.TaskID), orDash(ev.QueueID), eventSummary(ev))
	}
	if err != nil {
		return fmt.Errorf("cli: write event: %w", err)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// eventSummary derives a short human-readable description from the data.
func eventSummary(ev client.Event) string {
	switch ev.Type {
	case "task.progress":
		var d struct {
			Downloaded int64 `json:"downloaded"`
			Size       int64 `json:"size"`
			Speed      int64 `json:"speed"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			s := formatBytes(d.Downloaded)
			if d.Size > 0 {
				s += " / " + formatBytes(d.Size) + " (" + strconv.FormatInt(d.Downloaded*100/d.Size, 10) + "%)"
			}
			if d.Speed > 0 {
				s += " " + formatBytes(d.Speed) + "/s"
			}
			return s
		}
	case "task.status":
		var d struct {
			From  string `json:"from"`
			To    string `json:"to"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && (d.From != "" || d.To != "") {
			s := orDash(d.From) + " -> " + orDash(d.To)
			if d.Error != nil && d.Error.Message != "" {
				s += ": " + d.Error.Message
			}
			return s
		}
	}
	s := strings.TrimSpace(string(ev.Data))
	if r := []rune(s); len(r) > watchSummaryMax {
		s = string(r[:watchSummaryMax-3]) + "..."
	}
	return s
}

// formatBytes renders n with binary units, e.g. "1.5 MiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}
