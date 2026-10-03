package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/v2/config"
	nexusmanifest "github.com/paulmanoni/nexus/v2/manifest"

	// The framework packages that declare nexus.toml tables, linked so the
	// CLI checks [cache.*] / [storage.*] / [mail.*] / [jobs] like the app
	// does. (extension/config's [extensions.config] comes in through
	// extensions_for_lint.go.)
	_ "github.com/paulmanoni/nexus/v2/extension/cache"
	_ "github.com/paulmanoni/nexus/v2/extension/jobs"
	_ "github.com/paulmanoni/nexus/v2/extension/mail"
	_ "github.com/paulmanoni/nexus/v2/extension/storage"
)

func newConfigCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Check nexus.toml and print its JSON schema",
		Long: `nexus.toml is strict: every table is declared (by nexus, an extension, or
the app with config.Section) and every key in it is one its type has.
An undeclared table, a misspelt key or a key in the wrong table fails boot.

  nexus config check [path]    validate a file with the boot rules (CI)
  nexus config schema          print the JSON schema editors complete from`,
	}
	cmd.AddCommand(newConfigCheckCmd(stdout, stderr), newConfigSchemaCmd(stdout))
	return cmd
}

type configCheckOptions struct {
	path    string
	jsonOut bool
	noScan  bool
}

func newConfigCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts configCheckOptions
	cmd := &cobra.Command{
		Use:   "check [path]",
		Short: "Validate nexus.toml with the rules boot applies (exit 1 on a problem)",
		Long: `Validate nexus.toml (default: $NEXUS_CONFIG, else ./nexus.toml) with the
rules the app applies at boot: undeclared tables, unknown or misplaced keys,
[extensions.x] blocks without a decoder, wrong value types and bad values
(addresses, CIDRs, durations, listener scopes).

The app's own declarations are read from the Go module the file sits in:
config.Section[T]("name") declares [name] with T's keys, and
RegisterExtensionDecoder("name", …) declares [extensions.name]. ${VAR}
placeholders need not be set.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.path = args[0]
			}
			return runConfigCheck(stdout, stderr, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit the findings as JSON")
	cmd.Flags().BoolVar(&opts.noScan, "no-scan", false, "don't read the app's own sections from its Go source")
	return cmd
}

func defaultConfigPath() string {
	if p := os.Getenv("NEXUS_CONFIG"); p != "" {
		return p
	}
	return config.DefaultPath
}

func runConfigCheck(stdout, stderr io.Writer, opts configCheckOptions) error {
	if opts.path == "" {
		opts.path = defaultConfigPath()
	}
	if _, err := os.Stat(opts.path); err != nil {
		return fmt.Errorf("nexus config check: %w", err)
	}
	if !opts.noScan {
		declareProjectConfig(projectRootFor(opts.path))
	}
	issues, err := config.LintFile(opts.path)
	if err != nil {
		return fmt.Errorf("nexus config check: %w", err)
	}
	if opts.jsonOut {
		return emitJSON(stdout, issues)
	}
	errs, warns := count(issues)
	if len(issues) == 0 {
		fmt.Fprintf(stdout, "%s: ok\n", opts.path)
		return nil
	}
	sortIssuesForOutput(issues)
	for _, is := range issues {
		msg := is.Message
		if !strings.HasPrefix(msg, opts.path+":") {
			msg = fmt.Sprintf("%s: %s: %s", opts.path, is.Path, msg)
		}
		fmt.Fprintf(stdout, "%s  %s\n", padSeverity(is.Severity), msg)
	}
	fmt.Fprintf(stdout, "\n%d %s, %d %s\n", errs, pluralize("error", errs), warns, pluralize("warning", warns))
	if errs > 0 {
		return errExitNonZero
	}
	return nil
}

// projectRootFor is the directory of the Go module a config file belongs
// to: the nearest ancestor of its directory with a go.mod, else its own
// directory.
func projectRootFor(path string) string {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return filepath.Dir(path)
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

func newConfigSchemaCmd(stdout io.Writer) *cobra.Command {
	var out, dir string
	var framework bool
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON schema of nexus.toml",
		Long: `Print a JSON schema (draft 2020-12) for nexus.toml, generated from the
declared tables. Inside a project it includes the app's own sections (read
from its Go source); --framework prints only what nexus declares — the
schema published at ` + config.SchemaURL + `.

Point an editor at it with a first line in nexus.toml (Taplo / Even Better TOML):

  #:schema ` + config.SchemaURL,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !framework {
				declareProjectConfig(projectRootFor(filepath.Join(dir, config.DefaultPath)))
			}
			raw, err := config.JSONSchema()
			if err != nil {
				return err
			}
			raw = append(raw, '\n')
			if out == "" || out == "-" {
				_, err = stdout.Write(raw)
				return err
			}
			return os.WriteFile(out, raw, 0o644)
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "write the schema to this file instead of stdout")
	cmd.Flags().StringVar(&dir, "dir", ".", "project directory to read the app's sections from")
	cmd.Flags().BoolVar(&framework, "framework", false, "only the tables nexus and its extensions declare")
	return cmd
}

// lintConfigIssues runs the nexus.toml checks for `nexus lint`, with the
// app's sections declared from its source first.
func lintConfigIssues(path string) ([]nexusmanifest.Issue, error) {
	declareProjectConfig(projectRootFor(path))
	return config.LintFile(path)
}
