package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/v2/extension/auth"
)

func newAuthCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Inspect extension/auth's setup",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "check [nexus.toml]",
		Short: "Check nexus.toml's [auth] table and print the setup it gives",
		Long: `Check nexus.toml's [auth] table (default ./nexus.toml) the way boot does, and
print the effective setup of the accounts-and-sign-in path (auth.Config{Users: …}):
the schemes in the order they are tried, the default gate, sign-in pages and areas,
the built-in endpoints, and the session, throttle and password rules.

Exits 1 when the table has a problem — an unknown key, a scheme type nexus doesn't
have, a jwt scheme without a key.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path := "nexus.toml"
			if len(args) == 1 {
				path = args[0]
			}
			raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
			if err != nil {
				return err
			}
			out, err := auth.ExplainTOML(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			fmt.Fprint(stdout, out)
			return nil
		},
	})
	return cmd
}
