package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
)

// newBuildCmd builds `nexus build [main-package]`.
//
// Bundles the frontend (when present), generates the embed file for
// any islands.src/ contents, and shells out to `go build`. No
// deployment split — the framework produces a single binary.
func newBuildCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		outputPath string
		mainPkg    string
	)
	cmd := &cobra.Command{
		Use:   "build [main-package]",
		Short: "Bundle the frontend and compile everything into one binary",
		Long: `Build the app as a single binary.

Bundles the frontend if the project has one, writes the embed file so
the bundle ships inside the binary, then runs 'go build' on the main
package.

Handlers written with //@ annotations are registered for this build
without writing anything into your source tree. Run
'nexus generate handlers' to commit those registrations instead, which
is what a plain 'go build' / 'go install' / 'go test' needs.

Examples:
    nexus build
    nexus build -o ./bin/myapp
    nexus build ./cmd/server`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			pkg := "."
			if mainPkg != "" {
				pkg = mainPkg
			} else if len(args) > 0 {
				pkg = args[0]
			}
			return runSimpleBuild(simpleBuildOptions{
				Output:      outputPath,
				MainPackage: pkg,
				Stdout:      stdout,
				Stderr:      stderr,
			})
		},
	}
	cmd.Flags().StringVarP(&outputPath, "out", "o", "", "path to write the binary to (default: go build's own naming)")
	cmd.Flags().StringVar(&mainPkg, "package", "", "Go main package to build (defaults to '.')")
	// The positional argument says the same thing and is the documented
	// form, so --package stays only for the scripts that already use it.
	_ = cmd.Flags().MarkDeprecated("package", "pass the main package as an argument: nexus build ./cmd/server")
	return cmd
}

// runBinaryPrintMode invokes binaryPath with NEXUS_PRINT_MANIFEST=1 and
// captures stdout. The framework's print-mode short-circuit dumps the
// manifest JSON and exits before any listener binds, so the binary is
// safe to run inline for `nexus lint --binary` to consume.
func runBinaryPrintMode(binaryPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath)
	cmd.Env = append(os.Environ(), "NEXUS_PRINT_MANIFEST=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run %s with NEXUS_PRINT_MANIFEST=1: %w (stderr: %s)", binaryPath, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
