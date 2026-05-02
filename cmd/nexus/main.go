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
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is the CLI version printed by `nexus version`. Three
// resolution layers, in priority:
//
//  1. -ldflags "-X main.Version=v0.21.20" at release time
//  2. runtime/debug.ReadBuildInfo() — the module version stamped
//     by `go install github.com/paulmanoni/nexus/cmd/nexus@vX.Y.Z`,
//     or the VCS commit + dirty flag when the user ran
//     `go install ./cmd/nexus` against a local checkout.
//  3. "dev" — the literal placeholder for `go run ./cmd/nexus ...`
//     where neither ldflags nor BuildInfo carries a version.
//
// resolveVersion runs once at init() so the value is stable
// across subcommand invocations within one process (tests).
var Version = resolveVersion()

// resolveVersion walks the priority chain. Most users will hit
// case 2 — `go install ...@vX.Y.Z` puts the tag in BuildInfo's
// Main.Version, which is what we want printed.
func resolveVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	// Fallback: when running from a local checkout, Main.Version
	// is "(devel)" but vcs.revision/vcs.modified are populated.
	// Surface them so a developer's `nexus version` reflects the
	// actual binary they're running, not a confusing "dev".
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if rev != "" {
		return "dev-" + rev + modified
	}
	return "dev"
}

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
		newReconcileCmd(stdout, stderr),
		newDocsCmd(stdout, stderr),
		newAPIDocsCmd(stdout, stderr),
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