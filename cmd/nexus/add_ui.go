package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"

	"github.com/paulmanoni/nexus/v2/view/ui"
)

func newAddCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Copy framework pieces into the project to own them",
	}
	cmd.AddCommand(newAddUICmd(stdout, stderr))
	return cmd
}

func newAddUICmd(stdout, _ io.Writer) *cobra.Command {
	var dir, pkg string
	var force bool
	cmd := &cobra.Command{
		Use:   "ui <component>...",
		Short: "Vendor components of the view/ui kit into the project",
		Long: `Copy components of github.com/paulmanoni/nexus/v2/view/ui into the project,
for an app that wants to own and change them: each component's .templ file,
the components it renders, and the kit's shared pieces (base.go, ui.go with
Script and Loading, icon.templ, ui.js and ui.css).

Components: ` + strings.Join(ui.ComponentNames(), ", ") + `, or all. A component
function's name works too (datatable, rowactions, select, …).

The copy is its own package (--package, default: the directory's name) and
serves its ui.js/ui.css under /_ui/<package>/ — load them with its Script()
instead of the kit's. Existing files are kept unless --force. nexus dev and
nexus build compile the copied templates like any other.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := addUI(dir, pkg, args, force)
			if err != nil {
				return err
			}
			for _, f := range res.wrote {
				fmt.Fprintf(stdout, "wrote %s\n", displayRel(f))
			}
			for _, f := range res.kept {
				fmt.Fprintf(stdout, "kept  %s (exists; --force to overwrite)\n", displayRel(f))
			}
			fmt.Fprintf(stdout, "\nimport %q and load its assets in the document head:\n\n", res.importPath)
			fmt.Fprintf(stdout, "    @%s.Script()   // in place of the kit's ui.Script()\n\n", res.pkg)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "ui", "directory to copy into")
	cmd.Flags().StringVar(&pkg, "package", "", "package name of the copy (default: the directory's name)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files that already exist")
	return cmd
}

type addUIResult struct {
	wrote, kept []string
	pkg         string
	importPath  string
}

var (
	packageClause = regexp.MustCompile(`(?m)^package ui$`)
	goIdent       = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// addUI copies the named components into dir as package pkg.
func addUI(dir, pkg string, names []string, force bool) (*addUIResult, error) {
	files, err := ui.Vendor(names...)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if pkg == "" {
		pkg = strings.ToLower(strings.NewReplacer("-", "", ".", "").Replace(filepath.Base(abs)))
	}
	if !goIdent.MatchString(pkg) {
		return nil, fmt.Errorf("%q is not a Go package name — pass --package", pkg)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	res := &addUIResult{pkg: pkg, importPath: importPathOf(abs)}
	names = make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(abs, name)
		if _, err := os.Stat(path); err == nil && !force {
			res.kept = append(res.kept, path)
			continue
		}
		if err := os.WriteFile(path, vendorFile(name, files[name], pkg), 0o644); err != nil {
			return res, err
		}
		res.wrote = append(res.wrote, path)
	}
	return res, nil
}

const vendoredNote = "Copied from github.com/paulmanoni/nexus/v2/view/ui by `nexus add ui`; this copy is yours to change."

// vendorFile rewrites a kit file for package pkg: its package clause, and
// ui.go's asset prefix, so the copy serves its own ui.js/ui.css beside
// the kit's.
func vendorFile(name string, src []byte, pkg string) []byte {
	switch filepath.Ext(name) {
	case ".go", ".templ":
		out := packageClause.ReplaceAll(src, []byte("package "+pkg+"\n\n// "+vendoredNote))
		if name == "ui.go" {
			out = bytes.Replace(out, []byte(`const Prefix = "/_view/ui/"`), []byte(`const Prefix = "/_ui/`+pkg+`/"`), 1)
		}
		return out
	case ".js", ".css":
		return append([]byte("/* "+vendoredNote+" */\n"), src...)
	}
	return src
}

// importPathOf is dir's import path from the enclosing go.mod, or "" when
// there is none.
func importPathOf(dir string) string {
	root := findModuleRoot(dir)
	if root == "" {
		return ""
	}
	src, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	mod := modfile.ModulePath(src)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return mod
	}
	return mod + "/" + filepath.ToSlash(rel)
}
