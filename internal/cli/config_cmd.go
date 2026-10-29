package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func configValueText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = fmt.Sprint(e)
		}
		return strings.Join(parts, ",")
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func configTable(v *client.ConfigView, only string) output.Table {
	t := output.Table{Columns: []string{"key", "value", "source", "pending"}}
	for _, e := range v.Entries {
		if only != "" && e.Key != only && !strings.HasPrefix(e.Key, only+".") {
			continue
		}
		pending := ""
		if e.Pending != nil {
			pending = configValueText(e.Pending)
		}
		t.Rows = append(t.Rows, []string{e.Key, configValueText(e.Value), e.Source, pending})
	}
	return t
}

// parseConfigValue turns the text of a command line into the JSON value the
// type of the key asks for.
func parseConfigValue(typ, text string) (any, error) {
	switch typ {
	case "bool":
		b, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("want true or false, got %q", text)
		}
		return b, nil
	case "int":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("want a whole number, got %q", text)
		}
		return n, nil
	case "duration":
		if _, err := time.ParseDuration(text); err != nil {
			return nil, fmt.Errorf("want a duration such as 30s or 5m, got %q", text)
		}
		return text, nil
	case "list":
		out := []string{}
		for item := range strings.SplitSeq(text, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		return out, nil
	default:
		return text, nil
	}
}

func (a *app) newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change the daemon's boot configuration",
		Long: "The boot configuration (listen address, logging, engines, ...) is read when the daemon starts. 'dlm config get' shows what the " +
			"running daemon uses and where each value came from; 'dlm config set' writes the config file, and the change applies after " +
			"'dlm system restart'.",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:         "get [KEY]",
			Short:       "Show the boot configuration (all of it, or one key or group)",
			Example:     "  dlm config get\n  dlm config get log\n  dlm config get server.address",
			Args:        cobra.MaximumNArgs(1),
			Annotations: map[string]string{annotationOperationID: "get-config"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				v, err := c.GetConfig(cmd.Context())
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				only := ""
				if len(args) == 1 {
					only = args[0]
					var keep []client.ConfigEntry
					for _, e := range v.Entries {
						if e.Key == only || strings.HasPrefix(e.Key, only+".") {
							keep = append(keep, e)
						}
					}
					if len(keep) == 0 {
						return &usageError{err: fmt.Errorf("no configuration key %q", only)}
					}
					v.Entries = keep
				}
				return a.printer(cmd).Print(v, func() output.Table { return configTable(v, only) }) //nolint:wrapcheck // stdout write errors need no context
			},
		},
		a.newConfigSetCommand(),
	)
	return cmd
}

func (a *app) newConfigSetCommand() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "set KEY [VALUE]",
		Short: "Change a key in the config file",
		Long: "Write KEY=VALUE into the daemon's config file after checking the result. Durations are written like 30s, lists " +
			"comma-separated. --unset removes the key so its default applies again. The running daemon is not changed: restart it with " +
			"'dlm system restart'.",
		Example:     "  dlm config set log.level debug\n  dlm config set server.fs_roots /data,/media\n  dlm config set torrent.dht --unset",
		Args:        cobra.RangeArgs(1, 2),
		Annotations: map[string]string{annotationOperationID: "update-config"},
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			if unset != (len(args) == 1) {
				return &usageError{err: fmt.Errorf("give a VALUE, or --unset without one")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			var value any
			if !unset {
				cur, err := c.GetConfig(cmd.Context())
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				typ := ""
				for _, e := range cur.Entries {
					if e.Key == key {
						typ = e.Type
					}
				}
				if typ == "" {
					return &usageError{err: fmt.Errorf("no configuration key %q", key)}
				}
				if value, err = parseConfigValue(typ, args[1]); err != nil {
					return &usageError{err: fmt.Errorf("%s: %w", key, err)}
				}
			}
			v, err := c.UpdateConfig(cmd.Context(), map[string]any{key: value})
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if v.RestartPending && a.format == output.FormatTable {
				writeErr(cmd.ErrOrStderr(), "note: the change applies after a restart: dlm system restart")
			}
			return a.printer(cmd).Print(v, func() output.Table { return configTable(v, key) }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
	cmd.Flags().BoolVar(&unset, "unset", false, "remove the key from the config file")
	return cmd
}
