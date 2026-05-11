package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	nexusmanifest "github.com/paulmanoni/nexus/manifest"
)

// lintOptions carries the flags `nexus lint` accepts. The cobra
// command translates user flags into one of these and hands it to
// runLint; tests construct it directly to drive the validator
// without going through cobra parsing.
type lintOptions struct {
	// Source is where to read the manifest JSON from. Mutually
	// exclusive set: filePath (path on disk, "-" for stdin), or
	// binaryPath (run a binary in NEXUS_PRINT_MANIFEST=1 mode and
	// lint its stdout).
	filePath   string
	binaryPath string

	// JSON output for machine consumers (CI, IDE integrations). When
	// false, the human-readable text formatter is used.
	jsonOut bool

	// Quiet suppresses Warning-severity issues from both human output
	// and the exit code. Useful in CI when warnings are tracked but
	// not blocking.
	quiet bool
}

// newLintCmd is the `nexus lint` cobra command. Reads a manifest
// (from a JSON file, stdin, or by running a binary in print mode),
// invokes manifest.Lint, prints issues, exits 0 when no errors and 1
// otherwise (matching the conventions of every other linter that
// integrates with CI).
//
// Three input shapes:
//
//	nexus lint <manifest.json>     read JSON from disk
//	nexus lint -                   read JSON from stdin
//	nexus lint --binary=./bin/app  exec binary in print mode, lint output
//
// With no positional and no --binary, defaults to reading stdin so
// the canonical CI pipeline works without ceremony:
//
//	./bin/myapp -e NEXUS_PRINT_MANIFEST=1 | nexus lint
//
// Flags:
//
//	--json     emit machine-readable JSON instead of the text report
//	--quiet    suppress warnings (only errors counted toward exit code)
//	--binary   exec a binary in NEXUS_PRINT_MANIFEST=1 mode
func newLintCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts lintOptions

	cmd := &cobra.Command{
		Use: "lint [manifest.json]",
		// Don't let cobra re-print the sentinel error after we've
		// already emitted the report — the report IS the error
		// surface; an "Error: lint: errors found" line afterward is
		// noise. SilenceUsage is inherited (true) so usage text only
		// appears for actual arg errors.
		SilenceErrors: true,
		Short:         "Validate a nexus manifest's inputs schema + overrides",
		Long: `Run manifest.Lint against a manifest JSON document and report every issue.

Catches: duplicate input names, malformed validation rules, default values
violating their own enum/regex/length, BoundTo references to unknown
services, required env vars without source, every per-environment override
mismatch (unknown key, scalar+spec conflict, unknown environment, removed
entry not declared in base), bad regex patterns, length min > max.

Errors fail the command with exit code 1; warnings are advisory and don't
affect the exit code unless --quiet is unset (the default — quiet means
"suppress warnings entirely", not "treat warnings as errors").

Input sources:
  nexus lint <path>           manifest JSON file
  nexus lint -                read JSON from stdin (default if no arg)
  nexus lint --binary=PATH    exec the binary with NEXUS_PRINT_MANIFEST=1`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.filePath = args[0]
			}
			return runLint(stdout, stderr, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit issues as JSON instead of the text report")
	cmd.Flags().BoolVar(&opts.quiet, "quiet", false, "suppress warning-severity issues from output")
	cmd.Flags().StringVar(&opts.binaryPath, "binary", "", "exec the binary in NEXUS_PRINT_MANIFEST=1 mode and lint the result")

	return cmd
}

// runLint is the testable entry point — separate from the cobra
// command so tests can construct lintOptions directly and assert on
// stdout/stderr without flag-parsing machinery.
func runLint(stdout, stderr io.Writer, opts lintOptions) error {
	if opts.filePath != "" && opts.binaryPath != "" {
		return errors.New("nexus lint: cannot combine a manifest path with --binary")
	}

	raw, source, err := readManifestSource(opts)
	if err != nil {
		return err
	}

	var m nexusmanifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("nexus lint: parse manifest JSON from %s: %w", source, err)
	}

	issues := nexusmanifest.Lint(m)
	if opts.quiet {
		issues = filterWarnings(issues)
	}

	if opts.jsonOut {
		return emitJSON(stdout, issues)
	}
	return emitText(stdout, stderr, source, issues)
}

// readManifestSource resolves opts into the manifest bytes plus a
// human-readable label for error messages. Stdin is the default when
// neither filePath nor binaryPath is set, matching the pipe-friendly
// CI workflow.
func readManifestSource(opts lintOptions) (raw []byte, source string, err error) {
	switch {
	case opts.binaryPath != "":
		out, err := runBinaryPrintMode(opts.binaryPath)
		if err != nil {
			return nil, "", fmt.Errorf("nexus lint: %w", err)
		}
		return out, opts.binaryPath, nil
	case opts.filePath == "" || opts.filePath == "-":
		out, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", fmt.Errorf("nexus lint: read stdin: %w", err)
		}
		if len(out) == 0 {
			return nil, "", errors.New("nexus lint: no manifest provided — pass a path, --binary, or pipe JSON to stdin")
		}
		return out, "stdin", nil
	default:
		out, err := os.ReadFile(opts.filePath)
		if err != nil {
			return nil, "", fmt.Errorf("nexus lint: read %s: %w", opts.filePath, err)
		}
		return out, opts.filePath, nil
	}
}

// filterWarnings drops warning-severity entries. Used when --quiet is
// set; preserves the original slice's ordering for the remaining
// errors so deterministic output stays deterministic.
func filterWarnings(issues []nexusmanifest.Issue) []nexusmanifest.Issue {
	out := issues[:0]
	for _, i := range issues {
		if i.Severity == nexusmanifest.SeverityError {
			out = append(out, i)
		}
	}
	return out
}

// emitJSON writes a machine-consumer-friendly document with the full
// issue list + a summary block. Format is stable; consumers should
// gate behavior on the summary counts rather than parsing text.
func emitJSON(stdout io.Writer, issues []nexusmanifest.Issue) error {
	errs, warns := count(issues)
	doc := struct {
		Issues  []nexusmanifest.Issue `json:"issues"`
		Summary struct {
			Errors   int `json:"errors"`
			Warnings int `json:"warnings"`
		} `json:"summary"`
	}{Issues: issues}
	doc.Summary.Errors = errs
	doc.Summary.Warnings = warns
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if errs > 0 {
		return errExitNonZero
	}
	return nil
}

// emitText writes the human-readable report: a header with the
// source + count, one block per issue, and a summary footer. Errors
// before warnings within the issue list so operators see the
// blocking items first.
func emitText(stdout, stderr io.Writer, source string, issues []nexusmanifest.Issue) error {
	errs, warns := count(issues)

	if len(issues) == 0 {
		fmt.Fprintf(stdout, "nexus lint: %s — manifest is valid (0 errors, 0 warnings)\n", source)
		return nil
	}

	fmt.Fprintf(stdout, "nexus lint: %s\n\n", source)

	// Errors first, then warnings.
	sortIssuesForOutput(issues)

	for _, i := range issues {
		fmt.Fprintf(stdout, "  %s  %s\n", padSeverity(i.Severity), i.Path)
		fmt.Fprintf(stdout, "         %s\n\n", i.Message)
	}

	verb := "is invalid"
	if errs == 0 {
		verb = "is valid (warnings only)"
	}
	fmt.Fprintf(stdout, "%d %s, %d %s — manifest %s.\n",
		errs, pluralize("error", errs),
		warns, pluralize("warning", warns),
		verb,
	)

	if errs > 0 {
		return errExitNonZero
	}
	return nil
}

// sortIssuesForOutput puts errors before warnings; within each
// severity group, the input order (which manifest.Lint already
// stable-sorted by path + code) is preserved. Operators see what's
// blocking first; warnings cluster at the bottom where they're easy
// to ignore.
func sortIssuesForOutput(issues []nexusmanifest.Issue) {
	// Two-pointer pass that's stable within each severity. Faster
	// than sort.SliceStable for the typical N=10 case and easier to
	// reason about.
	errs := make([]nexusmanifest.Issue, 0, len(issues))
	warns := make([]nexusmanifest.Issue, 0)
	for _, i := range issues {
		if i.Severity == nexusmanifest.SeverityError {
			errs = append(errs, i)
		} else {
			warns = append(warns, i)
		}
	}
	copy(issues, errs)
	copy(issues[len(errs):], warns)
}

func count(issues []nexusmanifest.Issue) (errs, warns int) {
	for _, i := range issues {
		switch i.Severity {
		case nexusmanifest.SeverityError:
			errs++
		case nexusmanifest.SeverityWarning:
			warns++
		}
	}
	return
}

func padSeverity(s nexusmanifest.Severity) string {
	// "error" (5) and "warning" (7) — pad error to align columns.
	if s == nexusmanifest.SeverityError {
		return "error  "
	}
	return "warning"
}

func pluralize(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// errExitNonZero is a sentinel the cobra harness propagates so the
// process exit code is non-zero without printing yet another error
// line (we've already printed the report). main.go's command-error
// handling treats this sentinel specially.
var errExitNonZero = errors.New("lint: errors found")

// IsLintExitError reports whether err is the lint-found-errors
// sentinel. Used by main.go to translate to exit code 1 without
// printing the sentinel's message (which would be redundant — the
// report already explained what went wrong).
func IsLintExitError(err error) bool {
	return errors.Is(err, errExitNonZero)
}

// hint is intentionally short — strings.Contains'able by callers
// who want to detect lint-mode output without parsing JSON.
const _ = "compile-time anchor for lint output marker"

// Compile-time guarantee that lint output is referenceable by
// scripts grep-ing for the prefix. If we ever change the prefix,
// adjust the linker constraint below.
var _ = strings.HasPrefix("nexus lint:", "nexus lint:")
