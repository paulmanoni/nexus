package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// newGoToolCmd wraps a go subcommand (test, vet) with the overlay nexus
// build compiles through: the //nexus: handler registrations and the
// compiled .templ views, none of which need to be on disk.
func newGoToolCmd(tool string, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   tool + " [go " + tool + " flags] [packages]",
		Short: "go " + tool + " with nexus's generated code (handlers, views) overlaid",
		Long: fmt.Sprintf(`Run go %[1]s with the generated code nexus build compiles in — the
//nexus: handler registrations and the compiled .templ views — supplied as a
go build overlay, so nothing generated has to sit in the source tree.

Every argument is passed to go %[1]s unchanged:

  nexus %[1]s ./...
  nexus %[1]s -run TestPets -v ./pets`, tool),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			return runGoTool(tool, args, stdout, stderr)
		},
	}
}

func runGoTool(tool string, args []string, stdout, stderr io.Writer) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	overlay, cleanup, err := buildHandlerOverlay(cwd)
	if err != nil {
		return err
	}
	defer cleanup()
	c := exec.Command("go", append([]string{tool}, args...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
	c.Env = os.Environ()
	if overlay != "" {
		c.Env = append(c.Env, "GOFLAGS="+strings.TrimSpace(os.Getenv("GOFLAGS")+" -overlay="+overlay))
	}
	if err := c.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// go already reported what failed.
			return fmt.Errorf("go %s: %w", tool, errExitNonZero)
		}
		return err
	}
	return nil
}
