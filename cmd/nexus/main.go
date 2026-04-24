// Command nexus is the developer CLI for the nexus framework. It is a thin
// wrapper around a few common tasks — scaffolding a project, booting it with
// a friendly banner — that don't warrant pulling in a heavy CLI library.
//
// Install:
//
//	go install github.com/paulmanoni/nexus/cmd/nexus@latest
//
// Commands:
//
//	nexus new <dir>   Scaffold a minimal nexus app (go.mod + main.go + a module).
//	nexus dev [dir]   Run `go run` on the target package and open the dashboard.
//	nexus version     Print the CLI version.
package main

import (
	"fmt"
	"io"
	"os"
)

// Version is stamped in at release time via -ldflags. Defaults to "dev" so
// unreleased builds (`go run ./cmd/nexus ...`) still produce meaningful output.
var Version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "nexus:", err)
		os.Exit(1)
	}
}

// run is the entry point separated from main() so tests can drive the
// CLI without shelling out (they pass custom argv + stdout/stderr).
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return fmt.Errorf("missing command")
	}
	switch args[0] {
	case "new":
		return cmdNew(args[1:], stdout, stderr)
	case "dev":
		return cmdDev(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "nexus", Version)
		return nil
	case "help", "--help", "-h":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `nexus — developer CLI for the nexus framework.

Usage:
  nexus <command> [args]

Commands:
  new <dir>      Scaffold a minimal nexus app in <dir>.
  dev [dir]      Run the app (go run) with a boot banner and open the dashboard.
                 <dir> defaults to the current directory.
  version        Print the CLI version.
  help           Show this help.

Flags (scoped per command):
  nexus new  -module <path>     Override the go.mod module path
                                (default: derived from <dir>'s basename).
  nexus dev  -addr <host:port>  Dashboard address to probe and open
                                (default: :8080).
             -no-open           Don't auto-open the browser.
`)
}