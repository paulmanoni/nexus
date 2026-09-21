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
//	nexus new <dir>       Scaffold a new nexus app.
//	nexus dev [dir]       Rebuild and rerun the app on every save.
//	nexus build           Build the app into one binary.
//	nexus init [dir]      Add a frontend to an existing project.
//	nexus generate        Code generation: frontend bindings, handler registration.
//	nexus client          Write the typed JS/TS client SDK to disk.
//	nexus apidocs         Generate and serve API reference docs.
//	nexus docs [topic]    Show inline documentation; --web opens the README.
//	nexus routes          List the endpoints an app mounts.
//	nexus lint            Check a manifest's inputs and a nexus.toml's keys.
//	nexus doctor          Audit a manifest for configuration problems.
//	nexus pki             Issue mTLS certificates for the peer mesh.
//	nexus version         Print the CLI version.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
// Version is left UNINITIALIZED so a release-time
// -ldflags "-X main.Version=vX.Y.Z" value survives to runtime. init()
// fills the fallback (BuildInfo → vcs → "dev") only when no ldflags value
// was injected. Initializing it with `= resolveVersion()` would run at
// startup and clobber the linker value — silently breaking layer 1.
var Version string

func init() {
	if Version == "" {
		Version = resolveVersion()
	}
}

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
	// SilenceErrors is set on the root, so this is the one place an
	// error reaches the terminal. Before, cobra printed it too and
	// anything that wasn't lint/doctor/routes came out twice.
	cmd, err := newRootCmd(os.Stdout, os.Stderr).ExecuteC()
	if err == nil {
		return
	}
	// The lint-exit sentinel ("lint: errors found") would be noise
	// after the report the command already printed.
	if !IsLintExitError(err) {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if cmd != nil && isUsageError(err) {
			fmt.Fprintf(os.Stderr, "\nRun '%s --help' to see the accepted arguments and flags.\n", cmd.CommandPath())
		}
	}
	os.Exit(1)
}

// usageErrorPrefixes are the messages cobra and pflag produce when the
// command line itself is wrong, as opposed to the command running and
// failing. Only those get pointed at --help; a build failure should not.
var usageErrorPrefixes = []string{
	"unknown flag",
	"unknown shorthand flag",
	"unknown command",
	"flag needs an argument",
	"invalid argument",
	"required flag",
	"accepts ",
	"requires at least",
	"unknown topic",
}

func isUsageError(err error) bool {
	msg := err.Error()
	for _, p := range usageErrorPrefixes {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// newRootCmd builds the cobra command tree. Factored out so tests can
// drive the CLI in-process with their own stdout/stderr; main() just
// wires it to os.* and runs.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	// Keep registration order inside each group so "Start here" reads
	// new → dev → build instead of cobra's alphabetical build → dev → new.
	cobra.EnableCommandSorting = false

	root := &cobra.Command{
		Use:   "nexus",
		Short: "Developer CLI for the nexus framework",
		Long: `nexus is the developer CLI for the nexus Go framework.

New here? Three commands cover the whole loop:

  nexus new myapp     scaffold an app (asks about frontend, database, cache, auth)
  nexus dev           rebuild and rerun on every save; prints the URLs to open
  nexus build         bundle the frontend and compile everything into one binary

Then "nexus docs" lists short guides for each feature, and every command
explains itself with --help.`,
		Version: Version,
		// Runtime failures print the error alone; main() adds a --help
		// pointer for the errors that are actually about command usage.
		SilenceUsage: true,
		// main() is the single place an error is printed. Leaving this
		// false made cobra print it too, so most errors appeared twice.
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	// Match `nexus version`, which predates the --version flag.
	root.SetVersionTemplate("nexus {{.Version}}\n")
	// One spelling per concept, old spellings still accepted. Applies to
	// this command and every descendant, so each command declares only the
	// canonical flag and the alias needs no per-command wiring.
	root.SetGlobalNormalizationFunc(normalizeFlagName)

	// Grouping so the three commands a newcomer needs sit at the top of
	// `nexus --help` instead of being sorted under apidocs.
	root.AddGroup(
		&cobra.Group{ID: groupStart, Title: "Start here:"},
		&cobra.Group{ID: groupProject, Title: "Build on it:"},
		&cobra.Group{ID: groupInspect, Title: "Inspect and diagnose:"},
		&cobra.Group{ID: groupPeer, Title: "Peer mesh (mTLS):"},
	)

	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add(groupStart,
		newNewCmd(stdout, stderr),
		newDevCmd(stdout, stderr),
		newBuildCmd(stdout, stderr),
	)
	add(groupProject,
		newInitCmd(stdout, stderr),
		newGenerateCmd(stdout, stderr),
		newClientCmd(stdout, stderr),
		newAPIDocsCmd(stdout, stderr),
	)
	add(groupInspect,
		newDocsCmd(stdout, stderr),
		newRoutesCmd(stdout, stderr),
		newLintCmd(stdout, stderr),
		newDoctorCmd(stdout, stderr),
	)
	// PKI for the peer mesh (extension/peer mTLS).
	add(groupPeer, newPkiCmd(stdout, stderr))
	// Ungrouped, so cobra files it under "Additional Commands".
	root.AddCommand(newVersionCmd(stdout))
	return root
}

// Command group IDs for `nexus --help`.
const (
	groupStart   = "start"
	groupProject = "project"
	groupInspect = "inspect"
	groupPeer    = "peer"
)

// flagAliases maps a flag spelling that used to exist onto the canonical one.
// The CLI had --output on two commands and --out on eight, for the same idea;
// rather than break the scripts that used either, both parse to --out.
//
// This is the right mechanism for an alias: the flag is declared once and
// bound to one variable. Declaring two names against the same variable would
// silently let the last one parsed win.
var flagAliases = map[string]string{
	"output": "out",
	// --tsconfig was declared as a second flag bound to the same variable
	// as --jsconfig, so passing both silently dropped one. It is an alias.
	"tsconfig": "jsconfig",
}

func normalizeFlagName(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if canonical, ok := flagAliases[name]; ok {
		return pflag.NormalizedName(canonical)
	}
	return pflag.NormalizedName(name)
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
