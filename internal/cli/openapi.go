package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/daemon"
)

func (a *app) newOpenAPICommand() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi",
		Short: "Print the OpenAPI 3.1 document of the REST API",
		Long: "Print the OpenAPI document of the daemon's REST API, as the daemon serves it at /openapi.json, without " +
			"starting a daemon or contacting one. JSON by default; -o yaml prints YAML. Releases publish it as a " +
			"versioned artifact, which client generators (Dart, TypeScript, ...) read.",
		Example: "  dlm openapi > openapi.json\n  dlm openapi -o yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Default()
			srv, err := daemon.BuildServer(&cfg, api.Deps{Token: "openapi-document-only", Version: a.version})
			if err != nil {
				return err //nolint:wrapcheck // daemon errors are already descriptive
			}
			doc := srv.API().OpenAPI()
			var data []byte
			if a.format == output.FormatYAML {
				data, err = doc.YAML()
			} else {
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
}
