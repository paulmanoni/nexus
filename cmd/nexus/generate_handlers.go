package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// handlerKeywords is the //@ directive set `nexus generate handlers` consumes.
// Anything outside this set is left for other tools (or ignored).
var handlerKeywords = []string{
	"provide", "rest", "query", "mutation", "subscription", "ws", "worker", "auth", "use",
}

type handlersOptions struct {
	Root  string
	Out   string
	Check bool
}

// newGenerateHandlersCmd builds `nexus generate handlers`: scan //@ annotations
// across a package tree and (re)write one decorator-form registration file per
// package. The committed output is what plain `go build`/`go install` compile,
// keeping annotated apps installable without the nexus toolchain.
func newGenerateHandlersCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := handlersOptions{Root: ".", Out: handlerGenFileName}
	cmd := &cobra.Command{
		Use:   "handlers [dir]",
		Short: "Generate decorator-form handler registration from //@ annotations",
		Long: `Scan //@ annotations on handler/constructor functions and write one
` + "`nexus_handlers_gen.go`" + ` per package containing the matching decorate.*
registrations (decorate.Rest/Query/Provide/WS/Worker, with //@auth as options).

The generated file is ordinary committed Go, so a plain go build / go install
sees every route — the annotations are sugar, not a required build step.

Annotations:
    //@provide                        -> decorate.Provide(fn)
    //@rest <METHOD> <PATH>           -> decorate.Rest(method, path, fn, opts…)
    //@query / //@mutation            -> decorate.Query/Mutation(fn, opts…)
    //@subscription                   -> decorate.Subscription(fn, opts…)
    //@ws <PATH> <TYPE>               -> decorate.WS(path, type, fn, opts…)
    //@worker <NAME>                  -> decorate.Worker(name, fn)
    //@auth Required | Requires("X")  -> auth.Required()/auth.Requires("X") option

Examples:
    nexus generate handlers ./...        # scan from cwd
    nexus generate handlers ./handlers   # scan one tree
    nexus generate handlers --check      # CI gate: fail if files are stale`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.Root = args[0]
			}
			return runGenerateHandlers(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.Out, "out", opts.Out, "name of the generated file written into each annotated package")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "exit non-zero if any generated file is out of date (no writes)")
	return cmd
}

func runGenerateHandlers(opts handlersOptions, stdout, stderr io.Writer) error {
	results, err := allHandlerArtifacts(opts.Root, opts.Out)
	if err != nil {
		return fmt.Errorf("nexus generate handlers: %w", err)
	}

	if opts.Check {
		var drift []string
		for _, r := range results {
			cur, err := os.ReadFile(r.Path)
			if err != nil || string(cur) != string(r.Content) {
				drift = append(drift, r.Path)
			}
		}
		if len(drift) == 0 {
			fmt.Fprintf(stdout, "ok: handler codegen up to date (%d package(s))\n", len(results))
			return nil
		}
		fmt.Fprintln(stderr, "drift — run `nexus generate handlers`:")
		for _, d := range drift {
			fmt.Fprintf(stderr, "  %s\n", d)
		}
		return fmt.Errorf("handler codegen drift: %d file(s) out of date", len(drift))
	}

	written := 0
	for _, r := range results {
		// Byte-equal write is a no-op: skip so an IDE/CI watcher doesn't see a
		// fresh mtime on every run (mirrors `nexus generate frontend`).
		if cur, err := os.ReadFile(r.Path); err == nil && string(cur) == string(r.Content) {
			continue
		}
		if err := os.WriteFile(r.Path, r.Content, 0o644); err != nil {
			return fmt.Errorf("nexus generate handlers: write %s: %w", r.Path, err)
		}
		written++
		fmt.Fprintf(stdout, "  %s\n", r.Path)
	}
	fmt.Fprintf(stdout, "handler codegen: %d written, %d package(s) with registrations\n", written, len(results))
	return nil
}
