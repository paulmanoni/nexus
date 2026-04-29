// Command nexus is the developer CLI for the nexus framework. The
// command surface is built on spf13/cobra so subcommand help, flag
// parsing, completion, and grouping behave the way Go developers
// expect from kubectl/hugo/cobra-using tools they already know.
//
// Install:
//
//	go install github.com/paulmanoni/nexus/cmd/nexus@latest
//
// Subcommands:
//
//	nexus new <dir>       Scaffold a minimal nexus app.
//	nexus init [dir]      Add nexus.deploy.yaml to an existing project.
//	nexus dev [dir]       Run `go run` on the target package, open the dashboard.
//	nexus build           Build a deployment binary using overlay-driven shadow code.
//	nexus docs [topic]    Show inline documentation; --web opens the README.
//	nexus version         Print the CLI version.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Version is stamped in at release time via -ldflags. Defaults to "dev"
// so unreleased builds (`go run ./cmd/nexus ...`) still produce
// meaningful output.
var Version = "dev"

func main() {
	if err := newRootCmd(os.Stdout, os.Stderr).Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the cobra command tree. Factored out so tests can
// drive the CLI in-process with their own stdout/stderr; main() just
// wires it to os.* and runs.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "nexus",
		Short: "Developer CLI for the nexus framework",
		Long: `nexus is a developer CLI for the nexus Go framework.

Run a single binary as a monolith or split it into independent services
with the same commands. Each subcommand is documented under nexus help <cmd>.`,
		SilenceUsage:  true, // cobra already prints errors; no double-printing usage on every error
		SilenceErrors: false,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.AddCommand(
		newVersionCmd(stdout),
		newNewCmd(stdout, stderr),
		newInitCmd(stdout, stderr),
		newDevCmd(stdout, stderr),
		newBuildCmd(stdout, stderr),
		newGenerateCmd(stdout, stderr),
		newDocsCmd(stdout, stderr),
	)
	return root
}

func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Fprintln(stdout, "nexus", Version)
		},
	}
}