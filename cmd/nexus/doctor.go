package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	nexusmanifest "github.com/paulmanoni/nexus/manifest"
)

// doctorOptions mirrors lintOptions — same input sources, same
// flag names — because operators expect `nexus doctor` to behave
// like `nexus lint` plus deeper checks. The two commands share
// readManifestSource + resolveInputFormat + parseManifest from
// lint.go to keep the input pipelines identical.
type doctorOptions struct {
	filePath    string
	binaryPath  string
	inputFormat string // "" | "yaml" | "json"
	jsonOut     bool
	quiet       bool
}

// newDoctorCmd wires the `nexus doctor` cobra command. Same flag
// surface as `nexus lint`; the difference is the underlying check
// set — Doctor catches configuration coherence problems Lint
// doesn't, like "service declared but no expose_as" or "secret
// required with no validation rule."
//
//	nexus doctor <manifest.json>     read JSON from disk
//	nexus doctor <nexus.deploy.yaml> read YAML (auto-detected by extension)
//	nexus doctor -                   read from stdin
//	nexus doctor --binary=PATH       exec binary in NEXUS_PRINT_MANIFEST=1
//
// Exit codes: 0 when no errors, 1 when any error finding. Warnings
// never affect the exit code unless --quiet (which only suppresses
// the display, not the exit semantics — symmetric with `nexus
// lint`).
func newDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts doctorOptions

	cmd := &cobra.Command{
		Use:           "doctor [manifest]",
		SilenceErrors: true,
		Short:         "Audit a nexus manifest for configuration-coherence problems",
		Long: `Run configuration coherence checks against a nexus manifest.

Unlike ` + "`nexus lint`" + ` (which validates SHAPE — types, enum constraints,
unknown keys), doctor validates SETUP COHERENCE — does the configuration
actually hang together for runtime?

Checks include:
  - Required env / secret without Default or BoundTo (operator must supply)
  - BoundTo references a service that isn't declared
  - Service with no expose_as (platform doesn't know which env vars to fill)
  - Service with no env var binding (your code can't receive connection info)
  - Required secret with no Validation rule (typos pass the boot check)
  - Override block targets undeclared environment
  - Override keys not declared in the base manifest

Input sources:
  nexus doctor <manifest.json>      JSON manifest file
  nexus doctor <nexus.deploy.yaml>  YAML inputs surface (auto-detected by extension)
  nexus doctor -                    read from stdin (default JSON; use --yaml for YAML)
  nexus doctor --binary=PATH        exec binary in NEXUS_PRINT_MANIFEST=1`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.filePath = args[0]
			}
			return runDoctor(stdout, stderr, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit findings as JSON instead of the text report")
	cmd.Flags().BoolVar(&opts.quiet, "quiet", false, "suppress warning-severity findings from output")
	cmd.Flags().StringVar(&opts.binaryPath, "binary", "", "exec the binary in NEXUS_PRINT_MANIFEST=1 mode and check the result")

	var yamlIn, jsonIn bool
	cmd.Flags().BoolVar(&yamlIn, "yaml", false, "force YAML input parsing (overrides auto-detection)")
	cmd.Flags().BoolVar(&jsonIn, "json-in", false, "force JSON input parsing (overrides auto-detection)")
	cmd.PreRunE = func(_ *cobra.Command, _ []string) error {
		if yamlIn && jsonIn {
			return errors.New("nexus doctor: --yaml and --json-in are mutually exclusive")
		}
		switch {
		case yamlIn:
			opts.inputFormat = "yaml"
		case jsonIn:
			opts.inputFormat = "json"
		}
		return nil
	}
	return cmd
}

// runDoctor is the testable entry point. Resolves input from
// lint's existing helpers (readManifestSource / parseManifest) so
// the two commands stay in lockstep — a manifest doctor accepts
// is one lint accepts and vice versa.
func runDoctor(stdout, stderr io.Writer, opts doctorOptions) error {
	if opts.filePath != "" && opts.binaryPath != "" {
		return errors.New("nexus doctor: cannot combine a manifest path with --binary")
	}
	if opts.inputFormat == "yaml" && opts.binaryPath != "" {
		return errors.New("nexus doctor: --yaml is incompatible with --binary (binary print mode emits JSON)")
	}

	// Reuse lint's reader + parser via a lintOptions shim. Same
	// types, same code paths.
	raw, source, err := readManifestSource(lintOptions{
		filePath:    opts.filePath,
		binaryPath:  opts.binaryPath,
		inputFormat: opts.inputFormat,
	})
	if err != nil {
		return remapLintErrorTag(err, "doctor")
	}
	format := resolveInputFormat(lintOptions{
		filePath:    opts.filePath,
		binaryPath:  opts.binaryPath,
		inputFormat: opts.inputFormat,
	}, source)
	m, err := parseManifest(raw, format, source)
	if err != nil {
		return remapLintErrorTag(err, "doctor")
	}

	findings := nexusmanifest.Doctor(m)
	if opts.quiet {
		findings = filterDoctorWarnings(findings)
	}

	if opts.jsonOut {
		return emitDoctorJSON(stdout, findings)
	}
	return emitDoctorText(stdout, source, findings)
}

// filterDoctorWarnings drops warning-severity findings. Used when
// --quiet is set; preserves order for remaining errors.
func filterDoctorWarnings(in []nexusmanifest.Finding) []nexusmanifest.Finding {
	out := in[:0]
	for _, f := range in {
		if f.Severity == nexusmanifest.SeverityError {
			out = append(out, f)
		}
	}
	return out
}

func emitDoctorJSON(stdout io.Writer, findings []nexusmanifest.Finding) error {
	errs, warns := countFindings(findings)
	doc := struct {
		Findings []nexusmanifest.Finding `json:"findings"`
		Summary  struct {
			Errors   int `json:"errors"`
			Warnings int `json:"warnings"`
		} `json:"summary"`
	}{Findings: findings}
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

func emitDoctorText(stdout io.Writer, source string, findings []nexusmanifest.Finding) error {
	errs, warns := countFindings(findings)
	if len(findings) == 0 {
		fmt.Fprintf(stdout, "nexus doctor: %s — no findings (0 errors, 0 warnings)\n", source)
		return nil
	}

	fmt.Fprintf(stdout, "nexus doctor: %s\n\n", source)

	for _, f := range findings {
		fmt.Fprintf(stdout, "  %s  %s\n", padSeverity(f.Severity), f.Path)
		fmt.Fprintf(stdout, "         %s\n", f.Message)
		if f.Hint != "" {
			fmt.Fprintf(stdout, "         hint: %s\n", f.Hint)
		}
		fmt.Fprintln(stdout)
	}

	verb := "needs attention"
	if errs == 0 {
		verb = "is healthy (warnings only)"
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

func countFindings(in []nexusmanifest.Finding) (errs, warns int) {
	for _, f := range in {
		switch f.Severity {
		case nexusmanifest.SeverityError:
			errs++
		case nexusmanifest.SeverityWarning:
			warns++
		}
	}
	return
}

// remapLintErrorTag swaps the "nexus lint:" prefix for "nexus
// doctor:" when forwarding errors from the shared reader/parser
// helpers. Keeps log lines self-describing without duplicating
// those helpers.
func remapLintErrorTag(err error, tool string) error {
	msg := err.Error()
	const prefix = "nexus lint:"
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return fmt.Errorf("nexus %s:%s", tool, msg[len(prefix):])
	}
	return err
}
