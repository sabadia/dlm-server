package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/config"
)

// TestCLIParity enforces the REST + CLI parity rule: every OpenAPI operation
// served by `dlm serve` has a CLI command annotated with its operationId, and
// every annotation names an existing operation.
func TestCLIParity(t *testing.T) {
	cfg := config.Default()
	cfg.Home = t.TempDir()
	srv, err := buildServer(&cfg, api.Deps{Token: "parity-token", Version: "parity"})
	if err != nil {
		t.Fatal(err)
	}
	apiOps := openAPIOperationIDs(srv.API().OpenAPI())
	if len(apiOps) == 0 {
		t.Fatal("no operations found in the OpenAPI document")
	}
	cliOps := cliOperationIDs(t, NewRootCommand("parity"))

	var missing, unknown []string
	for id := range apiOps {
		if _, ok := cliOps[id]; !ok {
			missing = append(missing, id)
		}
	}
	for id, cmds := range cliOps {
		if _, ok := apiOps[id]; !ok {
			unknown = append(unknown, id+" (on "+strings.Join(cmds, ", ")+")")
		}
	}
	slices.Sort(missing)
	slices.Sort(unknown)
	if len(missing) > 0 {
		t.Errorf("API operations without a CLI command (add a command with Annotations[%q]):\n  %s",
			annotationOperationID, strings.Join(missing, "\n  "))
	}
	if len(unknown) > 0 {
		t.Errorf("CLI commands annotated with operations that do not exist:\n  %s", strings.Join(unknown, "\n  "))
	}
}

func openAPIOperationIDs(doc *huma.OpenAPI) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, item := range doc.Paths {
		for _, op := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace,
		} {
			if op != nil {
				ids[op.OperationID] = struct{}{}
			}
		}
	}
	return ids
}

// cliOperationIDs maps each annotated operationId to the commands naming it.
func cliOperationIDs(t *testing.T, root *cobra.Command) map[string][]string {
	t.Helper()
	ids := make(map[string][]string)
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if v, ok := c.Annotations[annotationOperationID]; ok {
			for id := range strings.SplitSeq(v, ",") {
				id = strings.TrimSpace(id)
				if id == "" {
					t.Errorf("command %q has an empty operationId in %q", c.CommandPath(), v)
					continue
				}
				ids[id] = append(ids[id], c.CommandPath())
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return ids
}

func TestCLIOperationIDsSplitsCommaSeparated(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{Use: "a", Annotations: map[string]string{annotationOperationID: "list-x, get-x"}})
	got := cliOperationIDs(t, root)
	for _, id := range []string{"list-x", "get-x"} {
		if len(got[id]) != 1 || got[id][0] != "root a" {
			t.Errorf("ids[%q] = %v, want [root a]", id, got[id])
		}
	}
}
