package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
)

// newInitCmd builds the `nexus init` subcommand. Used in an existing
// Go module that already imports the framework — drops a
// nexus.deploy.yaml in the current directory so the project can build
// per deployment with `nexus build --deployment X` and run split via
// `nexus dev --split`.
//
// Differs from `nexus new`: nexus new scaffolds a full project from
// scratch (go.mod + main.go + module.go + deploy.yaml). nexus init
// only adds the manifest, leaving everything else alone.
func newInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		dir   string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Add nexus.deploy.yaml to an existing project",
		Long: `Initialize a deployment manifest for the project at <dir> (default ".").

Scans the project for nexus.DeployAs(...) tags and pre-populates a
deployments block with one entry per discovered tag (auto-assigned
ports starting at 8080) plus a default monolith entry. The manifest
is heavily commented so it doubles as a walkthrough — edit it to
match your topology and use --deployment <name> in nexus build /
nexus dev --split.

Refuses to overwrite an existing nexus.deploy.yaml unless --force.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := dir
			if target == "" && len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				target = "."
			}
			return runInit(target, force, stdout)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to initialize (default '.')")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing nexus.deploy.yaml")
	return cmd
}

// runInit writes a deploy manifest into target. When the project
// already has nexus.DeployAs(...) tags, the manifest's deployments
// block is pre-populated; otherwise a monolith-only template lands
// (matching the `nexus new` shape).
//
// Separated from the cobra wrapper so tests can drive it directly.
func runInit(target string, force bool, stdout io.Writer) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("nexus init: %s: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("nexus init: %s is not a directory", abs)
	}

	dest := filepath.Join(abs, "nexus.deploy.yaml")
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("nexus init: %s already exists — pass --force to overwrite", dest)
		}
	}

	tags := discoverTagsForInit(abs, stdout)
	body := manifestForTags(tags)
	if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}

	fmt.Fprintf(stdout, "wrote %s\n", dest)
	if len(tags) == 0 {
		fmt.Fprintf(stdout, "\nNo nexus.DeployAs(...) tags found — manifest declares one monolith deployment.\n")
		fmt.Fprintf(stdout, "Tag a module with nexus.DeployAs(\"foo-svc\") to make it splittable; the\n")
		fmt.Fprintf(stdout, "manifest's comments walk through the rest.\n")
	} else {
		fmt.Fprintf(stdout, "\nDiscovered %d DeployAs tag(s); pre-populated as split units:\n", len(tags))
		for i, t := range tags {
			fmt.Fprintf(stdout, "  %s on port %d\n", t, 8080+i+1)
		}
		fmt.Fprintf(stdout, "\nNext:\n")
		fmt.Fprintf(stdout, "  nexus dev --split             # run every unit, one terminal\n")
		fmt.Fprintf(stdout, "  nexus build --deployment %s  # ./bin/%s\n", tags[0], tags[0])
	}
	return nil
}

// discoverTagsForInit walks the project for DeployAs declarations.
// Returns an empty slice on parse errors rather than failing — the
// init command should still leave a manifest behind even on a
// half-broken codebase, so the user has something to edit.
func discoverTagsForInit(dir string, stderr io.Writer) []string {
	pattern := dir
	if pattern == "" || pattern == "." {
		pattern = "./..."
	} else {
		pattern = "./..."
	}
	// discoverDeployTags expects a Go-style pattern relative to cwd.
	// Run with cwd=dir so "./..." picks up the project's own modules.
	prevWd, err := os.Getwd()
	if err == nil {
		_ = os.Chdir(dir)
		defer os.Chdir(prevWd)
	}
	tags, err := discoverDeployTags(pattern)
	if err != nil {
		fmt.Fprintf(stderr, "nexus init: warn: tag discovery failed: %v\n", err)
		return nil
	}
	sort.Strings(tags)
	return tags
}

// manifestForTags renders the on-disk manifest body. When tags is
// empty, returns the same starter template `nexus new` writes (so
// projects converge on one shape). When tags is non-empty,
// pre-populates each as a split-unit deployment with a sequential
// port and adds a peers entry per tag.
func manifestForTags(tags []string) string {
	if len(tags) == 0 {
		return tmplDeployYaml
	}
	var b bytes.Buffer
	b.WriteString(`# nexus.deploy.yaml — deployment topology for this app.
#
# Pre-populated by 'nexus init' from the DeployAs tags it discovered
# in this project. Edit ports / owns lists / peers as needed; the
# heavily-commented version of this file (with a walkthrough on how
# to split modules) lives at:
#
#   github.com/paulmanoni/nexus → cmd/nexus/new.go (tmplDeployYaml)
#
# Quick reference:
#   deployments[X].owns        modules local to this unit (empty = all)
#   deployments[X].port        listen port for this unit
#   peers[tag].url             HTTP base URL (defaults localhost:port)
#   peers[tag].timeout         per-call timeout
#   peers[tag].auth            { type: bearer, token: ${ENV} }

deployments:
  # Monolith owns every module by default — useful for local dev
  # and as a fallback when split mode isn't wanted.
  monolith:
    port: 8080
`)
	for i, tag := range tags {
		port := 8081 + i
		fmt.Fprintf(&b, "\n  %s:\n", tag)
		fmt.Fprintf(&b, "    owns: [%s]\n", moduleNameFromTag(tag))
		fmt.Fprintf(&b, "    port: %d\n", port)
	}
	b.WriteString("\npeers:\n")
	for _, tag := range tags {
		fmt.Fprintf(&b, "  %s:\n", tag)
		fmt.Fprintf(&b, "    timeout: 2s\n")
		fmt.Fprintf(&b, "    # url: ${%s_URL}\n", tagToEnvVarName(tag))
		fmt.Fprintf(&b, "    # auth:\n")
		fmt.Fprintf(&b, "    #   type: bearer\n")
		fmt.Fprintf(&b, "    #   token: ${%s_TOKEN}\n\n", tagToEnvVarName(tag))
	}
	return b.String()
}

// moduleNameFromTag is a best-guess "tag → module name" — strips a
// "-svc" suffix when present ("orders-svc" → "orders"), else returns
// the tag verbatim. The user fixes the owns list if the heuristic
// guesses wrong; `nexus build` errors clearly when owns names a
// module that doesn't exist.
func moduleNameFromTag(tag string) string {
	const suffix = "-svc"
	if len(tag) > len(suffix) && tag[len(tag)-len(suffix):] == suffix {
		return tag[:len(tag)-len(suffix)]
	}
	return tag
}

// tagToEnvVarName converts a DeployAs tag into the env-var stem
// shared by the runtime ("USERS_SVC_URL" / "USERS_SVC_TOKEN"). The
// suffix (_URL, _TOKEN) is appended by the caller so one helper
// covers both URL and auth-token paths.
func tagToEnvVarName(tag string) string {
	out := make([]byte, 0, len(tag))
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-32)
		case c == '-':
			out = append(out, '_')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
