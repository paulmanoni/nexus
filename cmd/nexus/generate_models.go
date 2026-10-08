package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newGenerateModelsCmd builds `nexus generate models`: the ORM's generated
// code, which nexus dev/build/test/vet/lsp and makemigrations overlay, on
// disk for a plain go build.
func newGenerateModelsCmd(stdout, stderr io.Writer) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "models [dir]",
		Short: "Write the ORM's model registrations and row scanners to disk",
		Long: `Write the ORM's generated code into the tree: one orm_scanners_gen.go per
package declaring models — an orm.Register[T]() for every type embedding
orm.Model[T] (nexus.Boot then binds and checks it, and makemigrations plans
it, with no orm.For), plus the row scanners and typed field sets of those
models and of every orm.For[T]().

nexus dev, build, test, vet, lsp and makemigrations overlay the same code,
so nothing needs writing for them; write it for a plain go build, go test or
go install. A package's init runs only when the app links it, so a model in
a package the app doesn't import is neither registered nor planned.

Examples:
    nexus generate models           # write the files
    nexus generate models --check   # CI gate: fail if a file is out of date`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			results, err := ormArtifacts(root, false)
			if err != nil {
				return fmt.Errorf("nexus generate models: %w", err)
			}
			return writeGenerated(results, check, "models", stdout, stderr)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit non-zero if any generated file is out of date (no writes)")
	return cmd
}
