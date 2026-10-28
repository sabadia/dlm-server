package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

// printSettings prints a settings layer (JSON/YAML: the object; table: one
// PATH VALUE row per field).
func (a *app) printSettings(cmd *cobra.Command, layer json.RawMessage) error {
	return a.printer(cmd).Print(layer, func() output.Table {
		return output.Table{Columns: []string{"path", "value"}, Rows: settingsRows(layer)}
	})
}

func (a *app) newSettingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Manage the global settings",
		Long: "Manage the global settings layer. Values not set globally inherit the built-in defaults " +
			"(`dlm settings defaults`); queues and tasks may override them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		a.newSettingsReadCommand("get", "Show the global settings layer", "get-settings", (*client.Client).GetSettings),
		a.newSettingsPatchCommand(false),
		a.newSettingsPatchCommand(true),
		a.newSettingsReplaceCommand(),
		a.newSettingsReadCommand("defaults", "Show the built-in default settings", "get-default-settings", (*client.Client).DefaultSettings),
		a.newSettingsSchemaCommand(),
	)
	return cmd
}

// settingsReadFunc is get-settings or get-default-settings.
type settingsReadFunc func(*client.Client, context.Context) (json.RawMessage, error)

func (a *app) newSettingsReadCommand(use, short, op string, call settingsReadFunc) *cobra.Command {
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: op},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			layer, err := call(c, cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printSettings(cmd, layer)
		},
	}
}

// newSettingsPatchCommand builds `settings set path=value...` or, with
// unset, `settings unset path...`; both send a JSON merge patch.
func (a *app) newSettingsPatchCommand(unset bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set PATH=VALUE...",
		Short: "Set global settings",
		Long: "Set global settings by dotted path. Values are parsed as JSON when valid (numbers, booleans, " +
			"objects, arrays, quoted strings), else taken as a string.",
		Example:     "  dlm settings set maxConnections=8 retry.maxAttempts=10 downloadDir=/data/downloads",
		Args:        cobra.MinimumNArgs(1),
		Annotations: map[string]string{annotationOperationID: "patch-settings"},
	}
	if unset {
		cmd.Use = "unset PATH..."
		cmd.Short = "Unset global settings (they inherit the defaults again)"
		cmd.Long = "Unset global settings by dotted path; they inherit the built-in defaults again."
		cmd.Example = "  dlm settings unset maxConnections retry.maxAttempts"
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		doc := settingsDoc{}
		var err error
		if unset {
			err = doc.apply(nil, args)
		} else {
			err = doc.apply(args, nil)
		}
		if err != nil {
			return &usageError{err: err}
		}
		patch, err := doc.raw()
		if err != nil {
			return err
		}
		c, err := a.newClient(cmd, client.DefaultTimeout)
		if err != nil {
			return err
		}
		layer, err := c.PatchSettings(cmd.Context(), patch)
		if err != nil {
			return err //nolint:wrapcheck // client errors are already user-facing
		}
		return a.printSettings(cmd, layer)
	}
	return cmd
}

func (a *app) newSettingsReplaceCommand() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:         "replace -f FILE",
		Short:       "Replace the global settings with a JSON or YAML file",
		Long:        "Replace the whole global settings layer with the settings object in FILE (JSON or YAML; '-' reads stdin).",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "replace-settings"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return &usageError{err: errors.New("--file is required")}
			}
			layer, err := readSettingsFile(file, cmd.InOrStdin())
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			out, err := c.ReplaceSettings(cmd.Context(), layer)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printSettings(cmd, out)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "settings file (JSON or YAML; '-' for stdin)")
	return cmd
}

// newSettingsSchemaCommand builds `settings schema`: the machine-readable
// description of every settings field (JSON/YAML: the document; table: one
// row per field).
func (a *app) newSettingsSchemaCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "schema",
		Short:       "Describe the settings fields (type, group, bounds, levels)",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-settings-schema"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			doc, err := c.SettingsSchema(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(doc, func() output.Table {
				return output.Table{Columns: []string{"path", "type", "group", "levels", "description"}, Rows: settingsSchemaRows(doc)}
			})
		},
	}
}

// settingsSchemaRows renders the get-settings-schema document as table rows.
func settingsSchemaRows(raw json.RawMessage) [][]string {
	var doc struct {
		Fields []struct {
			Path        string   `json:"path"`
			Type        string   `json:"type"`
			Group       string   `json:"group"`
			Description string   `json:"description"`
			Levels      []string `json:"levels"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return [][]string{{"(invalid)", string(raw), "", "", ""}}
	}
	rows := make([][]string, 0, len(doc.Fields))
	for _, f := range doc.Fields {
		rows = append(rows, []string{f.Path, f.Type, f.Group, strings.Join(f.Levels, ","), f.Description})
	}
	return rows
}
