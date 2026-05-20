package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newInitCmd builds the `nexus init` subcommand. Scaffolds the
// frontend pipeline (islands.src + main.go embed) into an existing
// Go project. `--frontend=vue|react` selects the framework.
func newInitCmd(stdout, _ io.Writer) *cobra.Command {
	var (
		dir      string
		force    bool
		frontend string
	)
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold frontend pipeline into an existing project",
		Long: `Initialize the project at <dir> (default ".") with frontend
scaffolding.

--frontend=vue|react adds islands.src/main.{ts,tsx} + App.{vue,tsx},
islands/index.html, and patches main.go to embed islands/ via
nexus.ServeFrontend.

Refuses to overwrite an existing islands.src/ unless --force.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := dir
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				target = "."
			}
			if frontend == "" {
				return fmt.Errorf("nexus init: --frontend is required (vue or react)")
			}
			if frontend != "vue" && frontend != "react" {
				return fmt.Errorf("nexus init: --frontend must be vue or react, got %q", frontend)
			}
			return runInitFrontend(target, frontend, force, stdout)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to initialize (default '.')")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing islands.src/")
	cmd.Flags().StringVar(&frontend, "frontend", "",
		"scaffold a frontend framework into an existing project: 'vue' or 'react'")
	return cmd
}
