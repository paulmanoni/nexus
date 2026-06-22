package main

import (
	"io"

	"github.com/spf13/cobra"
)

// newGenerateCmd builds the `nexus generate` parent that hosts the
// frontend type-tree generator.
func newGenerateCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Code-generation helpers",
	}
	cmd.AddCommand(
		newGenerateFrontendCmd(stdout, stderr),
		newGenerateHandlersCmd(stdout, stderr),
	)
	return cmd
}
