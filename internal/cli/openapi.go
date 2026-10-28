package cli

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/daemon"
)

func (a *app) newOpenAPICommand() *cobra.Command {
	var oas string
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "Print the OpenAPI 3.1 document of the REST API",
		Long: "Print the OpenAPI document of the daemon's REST API, as the daemon serves it at /openapi.json, without " +
			"starting a daemon or contacting one. JSON by default; -o yaml prints YAML. Releases publish it as a " +
			"versioned artifact, which client generators (Dart, TypeScript, ...) read. --oas 3.0 prints the OpenAPI " +
			"3.0.3 downgrade (served at /openapi-3.0.json) for generators that do not understand 3.1.",
		Example: "  dlm openapi > openapi.json\n  dlm openapi -o yaml\n  dlm openapi --oas 3.0 > openapi-3.0.json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Default()
			srv, err := daemon.BuildServer(&cfg, api.Deps{Token: "openapi-document-only", Version: a.version})
			if err != nil {
				return err //nolint:wrapcheck // daemon errors are already descriptive
			}
			doc := srv.API().OpenAPI()
			var data []byte
			switch {
			case oas == "3.0" && a.format == output.FormatYAML:
				data, err = doc.DowngradeYAML()
			case oas == "3.0":
				data, err = downgradedJSON(doc)
			case a.format == output.FormatYAML:
				data, err = doc.YAML()
			default:
				data, err = json.MarshalIndent(doc, "", "  ")
				data = append(data, '\n')
			}
			if err != nil {
				return fmt.Errorf("encode the OpenAPI document: %w", err)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err //nolint:wrapcheck // stdout write errors need no context
		},
	}
	cmd.Flags().StringVar(&oas, "oas", "3.1", "OpenAPI version to print: 3.1 or 3.0 (the 3.0.3 downgrade)")
	_ = cmd.RegisterFlagCompletionFunc("oas", cobra.FixedCompletions([]string{"3.1", "3.0"}, cobra.ShellCompDirectiveNoFileComp))
	cmd.PreRunE = func(*cobra.Command, []string) error {
		if oas != "3.1" && oas != "3.0" {
			return &usageError{err: fmt.Errorf("--oas must be 3.1 or 3.0, got %q", oas)}
		}
		return nil
	}
	return cmd
}

// downgradedJSON returns the OpenAPI 3.0.3 downgrade of doc as indented JSON
// with a trailing newline.
func downgradedJSON(doc *huma.OpenAPI) ([]byte, error) {
	raw, err := doc.Downgrade()
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller adds context
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err //nolint:wrapcheck // the caller adds context
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
