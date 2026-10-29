package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/tlsinit"
)

func (a *app) newTLSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tls",
		Short: "Set up TLS for a daemon other machines connect to",
	}
	var (
		hosts []string
		days  int
		force bool
		apply bool
		dir   string
	)
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Create a self-signed certificate and print its fingerprint",
		Long: "Create a self-signed certificate and key (cert.pem and key.pem in the config directory, the key readable by you only) for a daemon " +
			"that other machines connect to, and print the fingerprint clients pin. Name the hosts and addresses clients use with --host. " +
			"--apply also writes server.tls_cert and server.tls_key into the config file; the daemon reads them when it starts. " +
			"To accept remote clients also set server.allow_remote and a listen address (see 'dlm config set').",
		Example: "  dlm tls init --host nas.local --host 192.168.1.10 --apply\n" +
			"  dlm config set server.allow_remote true && dlm config set server.address 0.0.0.0:17171",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			target := dir
			if target == "" {
				target = cfg.Paths.ConfigDir
			}
			certPath, keyPath := filepath.Join(target, "cert.pem"), filepath.Join(target, "key.pem")
			res, err := tlsinit.Generate(tlsinit.Options{Hosts: hosts, Days: days})
			if err != nil {
				return fmt.Errorf("cli: %w", err)
			}
			if err := tlsinit.WriteFiles(certPath, keyPath, res, force); err != nil {
				return fmt.Errorf("cli: %w", err)
			}
			if apply {
				if err := cfg.Patch(map[string]any{"server.tls_cert": certPath, "server.tls_key": keyPath}); err != nil {
					return fmt.Errorf("cli: write the config file: %w", err)
				}
			}
			out := struct {
				Certificate string   `json:"certificate"`
				Key         string   `json:"key"`
				Fingerprint string   `json:"fingerprint"`
				NotAfter    string   `json:"notAfter"`
				Names       []string `json:"names"`
				Applied     bool     `json:"applied"`
			}{certPath, keyPath, res.Fingerprint, res.NotAfter.Format("2006-01-02"), res.Names, apply}
			return a.printer(cmd).Print(out, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"what", "value"}}
				t.Rows = append(t.Rows,
					[]string{"certificate", certPath}, []string{"key", keyPath},
					[]string{"fingerprint", res.Fingerprint}, []string{"valid until", out.NotAfter},
					[]string{"names", fmt.Sprint(res.Names)},
				)
				if apply {
					t.Rows = append(t.Rows, []string{"config file", "server.tls_cert and server.tls_key set; restart the daemon"})
				} else {
					t.Rows = append(t.Rows, []string{"next", "dlm config set server.tls_cert " + certPath + " && dlm config set server.tls_key " + keyPath})
				}
				return t
			})
		},
	}
	fl := initCmd.Flags()
	fl.StringArrayVar(&hosts, "host", nil, "a host name or IP address clients connect to (repeatable); localhost is always included")
	fl.IntVar(&days, "days", tlsinit.DefaultDays, "how many days the certificate is valid")
	fl.BoolVar(&force, "force", false, "replace an existing certificate and key")
	fl.BoolVar(&apply, "apply", false, "also set server.tls_cert and server.tls_key in the config file")
	fl.StringVar(&dir, "dir", "", "where to write the files (default: the config directory)")
	cmd.AddCommand(initCmd)
	return cmd
}
