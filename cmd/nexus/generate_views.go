package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

func newGenerateViewsCmd(stdout, stderr io.Writer) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "views [dir]",
		Short: "Compile reactive .templ views (package nexus/view) into Go",
		Long: `Compile every package under dir (default: the current directory) that has
.templ files, as one unit: each gets its *_templ.go and a view_gen.go
registering its //nexus:page components, detected shards and exposed types; a
main package gets view_imports_gen.go linking them.

nexus dev runs this on every .templ save and nexus build compiles through an
overlay, so running it by hand is only needed for a plain go build / go test
without the nexus CLI. --check exits non-zero when the files on disk are out
of date (a CI drift gate).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			if check {
				return checkViews(root, stdout)
			}
			changed, err := viewgen.Module(root)
			for _, path := range changed {
				fmt.Fprintf(stdout, "wrote %s\n", displayRel(path))
			}
			if err != nil {
				return err
			}
			if len(changed) == 0 {
				fmt.Fprintln(stdout, "views up to date")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit non-zero if a generated file is out of date (no writes)")
	return cmd
}

func checkViews(root string, stdout io.Writer) error {
	plan, err := viewgen.Generate(root)
	if err != nil {
		return err
	}
	var stale []string
	for path, content := range plan.Files {
		if old, err := os.ReadFile(path); err != nil || !bytes.Equal(old, content) {
			stale = append(stale, path)
		}
	}
	for _, path := range plan.Remove {
		if _, err := os.Stat(path); err == nil {
			stale = append(stale, path)
		}
	}
	if len(stale) == 0 {
		fmt.Fprintln(stdout, "views up to date")
		return nil
	}
	for _, path := range stale {
		fmt.Fprintf(stdout, "stale: %s\n", displayRel(filepath.Clean(path)))
	}
	return fmt.Errorf("%d generated view file(s) out of date — run nexus generate views", len(stale))
}
