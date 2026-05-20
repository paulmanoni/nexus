package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// frontendChoices / dbChoices / cacheChoices are the options the
// flags accept and the prompt walks through. "none" sits first so
// pressing Enter with no input picks the no-extra-deps path.
var (
	frontendChoices = []string{"none", "vue", "react"}
	dbChoices       = []string{"none", "postgres", "mysql", "sqlite"}
	cacheChoices    = []string{"none", "redis"}
	authChoices     = []string{"none", "oauth2"}
)

// newNewCmd builds the `nexus new` subcommand. Flags carry the
// non-interactive contract; --yes (alias --no-prompt) suppresses
// the prompt loop so CI / automation get deterministic behaviour
// regardless of whether stdin is a tty.
func newNewCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		modulePath string
		frontend   string
		db         string
		cache      string
		auth       string
		yes        bool
	)
	cmd := &cobra.Command{
		Use:   "new <dir>",
		Short: "Scaffold a nexus app (interactive — picks frontend / db / cache)",
		Long: `Scaffold a runnable nexus app in <dir>.

By default the command prompts for a frontend (vue or none),
database (postgres / mysql / sqlite / none), and cache (redis or
none) when stdin is a tty. Pass --frontend / --db / --cache to
skip the prompt for any axis, or --yes to take defaults across
the board (none everywhere — minimum viable scaffold).

Generated layout:

  ./go.mod ./main.go ./module.go ./nexus.deploy.yaml ./README.md
  resources/database.go     # only when --db is set
  resources/cache.go        # only when --cache is set
  web/                      # only when --frontend=vue is set
    package.json vite.config.ts index.html src/{main.ts,App.vue}
    dist/index.html         # placeholder so ServeFrontend boots

` + "`go mod tidy && nexus dev`" + ` then runs the app, opens the SPA via
vite's dev server (HMR) when one's scaffolded, and mounts the
dashboard at /__nexus/.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts := scaffoldOpts{
				Dir:        args[0],
				ModulePath: modulePath,
				Frontend:   frontend,
				DB:         db,
				Cache:      cache,
				Auth:       auth,
			}
			// Interactive prompts fill any axis the user didn't pin
			// via flags. Skipped on a non-tty stdin or when --yes is
			// passed — both signal the caller wants determinism.
			interactive := !yes && stdinIsTerminal()
			if interactive {
				if err := promptMissing(&opts, os.Stdin, stdout); err != nil {
					return err
				}
			}
			// Defaults so an unattended run still produces a valid
			// scaffold instead of erroring on empty axes.
			if opts.Frontend == "" {
				opts.Frontend = "none"
			}
			if opts.DB == "" {
				opts.DB = "none"
			}
			if opts.Cache == "" {
				opts.Cache = "none"
			}
			if opts.Auth == "" {
				opts.Auth = "none"
			}
			return scaffoldWithOpts(opts, stdout)
		},
	}
	cmd.Flags().StringVar(&modulePath, "module", "",
		"go.mod module path (default: derived from <dir>'s basename)")
	cmd.Flags().StringVar(&frontend, "frontend", "",
		"frontend stack: "+strings.Join(frontendChoices, " | ")+" (default: prompt)")
	cmd.Flags().StringVar(&db, "db", "",
		"database driver: "+strings.Join(dbChoices, " | ")+" (default: prompt)")
	cmd.Flags().StringVar(&cache, "cache", "",
		"cache backend: "+strings.Join(cacheChoices, " | ")+" (default: prompt)")
	cmd.Flags().StringVar(&auth, "auth", "",
		"authentication: "+strings.Join(authChoices, " | ")+" (default: prompt)")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"skip prompts and accept defaults on any axis not passed via flags")
	return cmd
}

// scaffold (legacy entry point) preserves the original signature so
// the existing test suite keeps working. New callers go through
// scaffoldWithOpts directly.
func scaffold(dir, modulePath string, stdout io.Writer) error {
	return scaffoldWithOpts(scaffoldOpts{
		Dir:        dir,
		ModulePath: modulePath,
		Frontend:   "none",
		DB:         "none",
		Cache:      "none",
		Auth:       "none",
	}, stdout)
}

// scaffoldWithOpts is the real worker. Validates the directory +
// module path, renders every per-option template, writes them with
// MkdirAll-on-parent so subdirs (resources/, web/src/) exist, and
// prints a follow-up command list tailored to the chosen options.
func scaffoldWithOpts(opts scaffoldOpts, stdout io.Writer) error {
	if opts.Dir == "" {
		return fmt.Errorf("directory is required")
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return err
	}
	if opts.ModulePath == "" {
		opts.ModulePath = filepath.Base(abs)
	}
	if !isValidModulePath(opts.ModulePath) {
		return fmt.Errorf("module path %q is not a valid Go module path", opts.ModulePath)
	}
	opts.Name = filepath.Base(abs)

	if err := validChoice(opts.Frontend, "--frontend", frontendChoices); err != nil {
		return err
	}
	if err := validChoice(opts.DB, "--db", dbChoices); err != nil {
		return err
	}
	if err := validChoice(opts.Cache, "--cache", cacheChoices); err != nil {
		return err
	}
	if err := validChoice(opts.Auth, "--auth", authChoices); err != nil {
		return err
	}

	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		entries, _ := os.ReadDir(abs)
		if len(entries) > 0 {
			return fmt.Errorf("%s already exists and is not empty — refusing to overwrite", abs)
		}
	} else if err == nil {
		return fmt.Errorf("%s exists and is not a directory", abs)
	}

	if err := os.MkdirAll(abs, 0o750); err != nil {
		return err
	}
	files, err := buildFiles(opts)
	if err != nil {
		return err
	}
	for relPath, content := range files {
		full := filepath.Join(abs, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}
	}

	fmt.Fprintf(stdout, "Scaffolded %s (module %s)\n", abs, opts.ModulePath)
	fmt.Fprintf(stdout, "  frontend: %s\n", opts.Frontend)
	fmt.Fprintf(stdout, "  db:       %s\n", opts.DB)
	fmt.Fprintf(stdout, "  cache:    %s\n", opts.Cache)
	fmt.Fprintf(stdout, "  auth:     %s\n", opts.Auth)
	fmt.Fprintf(stdout, "\nNext:\n")
	for _, line := range nextStepsLines(opts) {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout)
	return nil
}

// promptMissing fills opts.Frontend / opts.DB / opts.Cache by asking
// the user. Already-set fields skip their prompt so flag-driven
// invocations short-circuit cleanly.
func promptMissing(opts *scaffoldOpts, stdin io.Reader, stdout io.Writer) error {
	r := bufio.NewReader(stdin)
	if opts.Frontend == "" {
		v, err := pickOne(r, stdout, "Frontend?", frontendChoices, 0)
		if err != nil {
			return err
		}
		opts.Frontend = v
	}
	if opts.DB == "" {
		v, err := pickOne(r, stdout, "Database?", dbChoices, 0)
		if err != nil {
			return err
		}
		opts.DB = v
	}
	if opts.Cache == "" {
		v, err := pickOne(r, stdout, "Cache?", cacheChoices, 0)
		if err != nil {
			return err
		}
		opts.Cache = v
	}
	if opts.Auth == "" {
		v, err := pickOne(r, stdout, "Authentication?", authChoices, 0)
		if err != nil {
			return err
		}
		opts.Auth = v
	}
	return nil
}

// pickOne prints a numbered menu, reads a line of stdin, and
// returns the matching choice. Empty input picks defIdx — that's
// why "none" sits first in every choice slice (Enter does the
// least-surprising thing). Invalid input re-prompts up to 3 times
// before giving up so a typo doesn't kill the scaffold.
func pickOne(r *bufio.Reader, w io.Writer, label string, choices []string, defIdx int) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(w, "\n%s\n", label)
		for i, c := range choices {
			marker := " "
			if i == defIdx {
				marker = "*"
			}
			fmt.Fprintf(w, "  [%s] %d) %s\n", marker, i+1, c)
		}
		fmt.Fprintf(w, "→ choose [%d]: ", defIdx+1)

		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return choices[defIdx], nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return choices[defIdx], nil
		}
		// Accept either an index ("2") or the full name ("postgres").
		if n, err := strconv.Atoi(line); err == nil {
			if n >= 1 && n <= len(choices) {
				return choices[n-1], nil
			}
		}
		for _, c := range choices {
			if strings.EqualFold(c, line) {
				return c, nil
			}
		}
		fmt.Fprintf(w, "  → %q is not one of the listed choices, try again\n", line)
	}
	return "", fmt.Errorf("too many invalid attempts at %q", label)
}

// stdinIsTerminal returns true when stdin is a real tty. Non-tty
// stdin (CI pipes, automated scripts) skips the prompt loop so
// `nexus new ... < /dev/null` doesn't hang. Implemented with the
// stdlib's mode bits to avoid pulling in golang.org/x/term.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// isValidModulePath is a loose check — enough to catch typos ("my app"
// with spaces) without replicating the full spec. `go mod init` will
// still reject anything subtly wrong; this is a pre-flight guard.
func isValidModulePath(p string) bool {
	if p == "" || strings.ContainsAny(p, " \t\n") {
		return false
	}
	for _, r := range p {
		if r < 0x20 {
			return false
		}
	}
	return true
}
