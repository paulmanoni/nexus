package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newInitCmd builds the `nexus init` subcommand: it adds a Vite
// frontend (web/) to an existing Go project and wires main.go to embed
// and serve it. `--frontend=vue|react` selects the framework.
func newInitCmd(stdout, _ io.Writer) *cobra.Command {
	var (
		dir      string
		force    bool
		frontend string
	)
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Add a Vite frontend (web/) to an existing project",
		Long: `Add a frontend to the project at <dir> (default ".").

--frontend=vue|react writes the same web/ project nexus new scaffolds —
package.json, vite.config.ts (with nexus-vite-plugin), tsconfig.json,
index.html, src/main.{ts,tsx} + App.{vue,tsx}, sdk/nexus-vite-plugin.{js,d.ts}
and a committed dist/index.html stub — and patches main.go to embed
web/dist and serve it with nexus.ServeFrontend. nexus dev installs the
dependencies on its first run (npm; Node.js 20+).

Refuses an existing web/ unless --force. With --force the project files
(package.json, vite.config.ts, tsconfig.json, sdk/) are rewritten — an
existing one that differs is saved first as <file>.orig — and the app's
own files (index.html, src/, the dist stub) are kept where they exist:
the way to move a viteless-era web/ onto Vite.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := dir
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				target = "."
			}
			if frontend != "vue" && frontend != "react" {
				return fmt.Errorf("nexus init: --frontend must be vue or react, got %q", frontend)
			}
			return runInitFrontend(target, frontend, force, stdout)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to initialize (default '.')")
	// The positional [dir] says the same thing and is the documented form.
	_ = cmd.Flags().MarkDeprecated("dir", "pass the directory as an argument: nexus init ./myproject")
	cmd.Flags().BoolVar(&force, "force", false, "add the Vite project files to an existing web/ (sources are kept; replaced config files are saved as <file>.orig)")
	cmd.Flags().StringVar(&frontend, "frontend", "",
		"frontend framework to scaffold: 'vue' or 'react'")
	_ = cmd.MarkFlagRequired("frontend")
	return cmd
}
