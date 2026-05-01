package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/internal/apidocs"
)

// newAPIDocsCmd builds `nexus apidocs <subcommand>` — the auto-
// generated API reference (Sphinx-style: services, endpoints,
// resources). Distinct from `nexus docs`, which prints the
// hand-written inline man pages.
//
// Today only the `build` subcommand exists, emitting IR JSON. HTML
// + PDF renderers will land as sibling subcommands so this top-level
// command becomes the umbrella for the whole pipeline.
func newAPIDocsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apidocs",
		Short: "Generate API reference docs from a nexus app",
		Long: `Generate Sphinx-style API reference docs by static analysis.

Scans a nexus app's Go source for Module / AsQuery / AsMutation /
AsRest / AsWS calls, follows each constructor reference, and emits
an intermediate representation (IR) describing every endpoint —
its doc comment, dependencies, args, return type, and options.

Subcommands will render the IR to HTML and PDF; today only the
collector ("build") is exposed.`,
	}
	cmd.AddCommand(
		newAPIDocsBuildCmd(stdout, stderr),
		newAPIDocsServeCmd(stdout, stderr),
	)
	return cmd
}

// newAPIDocsServeCmd starts a tiny local HTTP server that re-runs the
// collector on every page load and renders HTML. Decoupled from the
// app process — works on any nexus app without starting it.
//
// The serve loop is request-scoped: each GET / re-collects so changes
// to source files show up after a refresh, no watcher needed for v0.
// Cost is one go/packages.Load per request; fine for dev.
func newAPIDocsServeCmd(stdout, stderr io.Writer) *cobra.Command {
	var addr string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "serve [path]",
		Short: "Serve the API reference at a local URL",
		Long: `Starts a local HTTP server rendering the API reference.

Re-runs the collector on every request, so refreshing the page
picks up source-file edits without restarting. Use Ctrl-C to
stop the server.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := "."
			if len(args) == 1 {
				target = args[0]
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", target, err)
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", addr, err)
			}
			url := "http://" + ln.Addr().String() + "/"
			mux := http.NewServeMux()
			mux.HandleFunc("/", apidocsHandler(abs, "html", stderr))
			mux.HandleFunc("/ir.json", apidocsHandler(abs, "json", stderr))
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

			fmt.Fprintf(stdout, "nexus apidocs serving %s\n  HTML: %s\n  JSON: %sir.json\n",
				abs, url, url)
			if !noOpen {
				_ = openBrowser(url)
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			errCh := make(chan error, 1)
			go func() { errCh <- srv.Serve(ln) }()
			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
				fmt.Fprintln(stdout, "shutting down")
				return nil
			case err := <-errCh:
				if err != nil && err != http.ErrServerClosed {
					return err
				}
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "listen address (host:port)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open the browser automatically")
	return cmd
}

// apidocsHandler returns an http.Handler that re-collects the IR for
// `dir` and writes it as HTML or JSON. Errors render as plain text so
// a malformed source tree surfaces in the browser, not just the
// terminal.
func apidocsHandler(dir, format string, errOut io.Writer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc, err := apidocs.Collect(dir, "./...")
		if err != nil {
			fmt.Fprintf(errOut, "collect: %v\n", err)
			http.Error(w, "collect failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if format == "json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		body, err := apidocs.RenderHTML(doc)
		if err != nil {
			http.Error(w, "render failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}
}


// newAPIDocsBuildCmd emits the IR as JSON. Defaults to stdout so
// `nexus apidocs build ./examples/graphapp | jq` works without
// flags; -o writes to a file instead.
func newAPIDocsBuildCmd(stdout, stderr io.Writer) *cobra.Command {
	var output string
	var pretty bool
	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Collect API IR from the package at [path] (default: .)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := "."
			if len(args) == 1 {
				target = args[0]
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", target, err)
			}
			doc, err := apidocs.Collect(abs, "./...")
			if err != nil {
				return err
			}
			var buf []byte
			if pretty {
				buf, err = json.MarshalIndent(doc, "", "  ")
			} else {
				buf, err = json.Marshal(doc)
			}
			if err != nil {
				return fmt.Errorf("encode IR: %w", err)
			}
			w := stdout
			if output != "" {
				f, err := os.Create(output)
				if err != nil {
					return fmt.Errorf("create %s: %w", output, err)
				}
				defer f.Close()
				w = f
			}
			if _, err := w.Write(buf); err != nil {
				return err
			}
			if output != "" {
				fmt.Fprintf(stderr, "wrote %s\n", output)
			} else {
				fmt.Fprintln(w)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write IR to file instead of stdout")
	cmd.Flags().BoolVar(&pretty, "pretty", true, "indent JSON for readability")
	return cmd
}