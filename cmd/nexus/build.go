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
		Short: "Build a single binary",
		Long: `Build the app as a single binary.

Runs the frontend bundler on any islands.src/ sources, generates the
embed file, and shells out to 'go build' on the main package.

Examples:
    nexus build
    nexus build -o ./bin/myapp
    nexus build ./cmd/server`,
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
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "output binary path; defaults to go build's default")
	cmd.Flags().StringVar(&mainPkg, "package", "", "Go main package to build (defaults to '.')")
	return cmd
}

// hasGoSuffix reports whether name ends in ".go".
func hasGoSuffix(name string) bool {
	return len(name) > 3 && name[len(name)-3:] == ".go"
}

// isGeneratedName reports whether a file is a "zz_*" generated source.
func isGeneratedName(name string) bool {
	return len(name) >= 3 && name[:3] == "zz_"
}

// relTo returns target relative to base, falling back to target on
// any path resolution error.
func relTo(base, target string) string {
	rel, err := filepathRel(base, target)
	if err != nil {
		return target
	}
	return rel
}

// filepathRel wraps filepath.Rel so build.go can avoid importing
// path/filepath in this trimmed-down form. Kept for parity with the
// pre-split helpers other files in cmd/nexus still rely on.
func filepathRel(base, target string) (string, error) {
	if base == "" {
		return target, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		_ = wd
	}
	if len(target) > len(base) && target[:len(base)] == base {
		out := target[len(base):]
		if len(out) > 0 && (out[0] == '/' || out[0] == '\\') {
			out = out[1:]
		}
		return out, nil
	}
	return target, nil
}

// joinArgs is used by error formatting elsewhere; preserved for
// callers that imported it before the split removal.
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
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

