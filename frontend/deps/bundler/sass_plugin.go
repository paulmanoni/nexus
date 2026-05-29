package bundler

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// NewSassPlugin returns an esbuild plugin that compiles .scss /
// .sass files by shelling out to the system `sass` CLI. Result
// CSS is handed back to esbuild's native CSS pipeline, which
// then bundles + emits it alongside the JS.
//
// Design note: we deliberately don't bundle a Sass compiler
// into the nexus binary — dart-sass (the canonical implementation
// today) is megabytes of JS-via-Node or megabytes of WASM, and
// most projects either:
//
//   - already have `sass` installed (npm install sass, brew
//     install sass, or via a Vue/React project's existing
//     toolchain), or
//   - can switch to plain CSS — modern CSS nesting + custom
//     properties cover ~90% of what most SCSS files actually
//     use.
//
// Shelling out means we get the canonical compiler with zero
// integration risk, at the cost of one process exec per file.
// Acceptable for the typical 1-5 SCSS files per project; if it
// becomes a bottleneck the right fix is upstream caching, not
// in-process compilation.
//
// When the `sass` binary isn't on PATH, the plugin surfaces a
// clear actionable error with three resolution paths (install
// dart-sass, install via npm, or convert to plain CSS) instead
// of esbuild's default "no loader configured" which is
// confusing for operators who DID install sass globally but
// not in this project.
func NewSassPlugin() api.Plugin {
	return api.Plugin{
		Name: "nexus-sass",
		Setup: func(build api.PluginBuild) {
			// Resolve .scss / .sass to absolute paths in a
			// custom namespace so OnLoad fires. We use the
			// default resolver for the path itself — sass
			// imports look like regular relative paths.
			build.OnResolve(api.OnResolveOptions{Filter: `\.s[ac]ss$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				absPath := args.Path
				if !filepath.IsAbs(absPath) {
					absPath = filepath.Join(args.ResolveDir, args.Path)
				}
				return api.OnResolveResult{
					Path:      absPath,
					Namespace: "nexus-sass",
				}, nil
			})
			build.OnLoad(api.OnLoadOptions{
				Filter:    `.*`,
				Namespace: "nexus-sass",
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return compileSass(args.Path)
			})
		},
	}
}

// compileSass invokes `sass <path>` and returns its stdout as
// CSS. Stderr is captured into the error message so operators
// see the actual Sass diagnostic (line/column + source snippet)
// rather than a generic "exit 65".
//
// Returns the loader as `css` so esbuild's native CSS bundling
// picks up the result — @import + url() recurse, font/image
// references get resolved relative to the SCSS file's dir.
//
// The sass binary lookup is done per-invocation (not cached at
// plugin construction) so adding sass mid-session via brew/npm
// install starts working without a CLI restart.
func compileSass(path string) (api.OnLoadResult, error) {
	sassBin, err := exec.LookPath("sass")
	if err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-sass: %q requires the `sass` CLI, but it's not on PATH", path),
				Notes: []api.Note{
					{Text: "Install dart-sass: `brew install sass` (macOS) or download from https://sass-lang.com/install"},
					{Text: "Or via npm in your project: `npm install -g sass`"},
					{Text: "Or convert this file to plain .css — modern CSS nesting + custom properties cover most SCSS uses"},
				},
			}},
		}, nil
	}
	// --no-source-map: esbuild will regenerate a source map for
	//   the bundled output; sass's per-file maps would point at
	//   .scss paths that don't exist post-bundle.
	// --style=expanded: readable CSS for dev builds; esbuild
	//   minifies after if Options.Minify is set.
	cmd := exec.Command(sassBin, "--no-source-map", "--style=expanded", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-sass: compile %s failed", path),
				Notes: []api.Note{
					{Text: stderr.String()},
				},
			}},
		}, nil
	}
	css := stdout.String()
	loader := api.LoaderCSS
	return api.OnLoadResult{
		Contents:   &css,
		Loader:     loader,
		ResolveDir: filepath.Dir(path),
	}, nil
}

// CompileSassSource compiles an in-memory SCSS/Sass string to CSS by
// piping it through the system `sass` CLI's --stdin mode. Used for
// inline SFC `<style lang="scss">` blocks, which never hit the
// file-based OnLoad path above (they're embedded in .vue source, not
// imported as .scss files).
//
// loadPath is added as a --load-path so `@use`/`@import` inside the
// block resolve relative to the .vue file's directory. Pass
// indented=true for `lang="sass"` (the indented syntax); scss is the
// default.
//
// The sass binary lookup is per-invocation (not cached) so installing
// sass mid-session starts working without a restart, matching
// compileSass above.
func CompileSassSource(src, loadPath string, indented bool) (string, error) {
	sassBin, err := exec.LookPath("sass")
	if err != nil {
		return "", fmt.Errorf("the `sass` CLI is required for inline <style lang=\"scss\"> but is not on PATH — " +
			"install dart-sass (`brew install sass` or `npm install -g sass`), or convert the block to plain CSS")
	}
	args := []string{"--stdin", "--no-source-map", "--style=expanded"}
	if loadPath != "" {
		args = append(args, "--load-path="+loadPath)
	}
	if indented {
		args = append(args, "--indented")
	}
	cmd := exec.Command(sassBin, args...)
	cmd.Stdin = bytes.NewReader([]byte(src))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("sass compile failed: %s", strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// sassAvailable reports whether the system `sass` CLI is on
// PATH. Used by the CLI to decide whether to register the
// plugin at all — if sass isn't installed, projects without
// .scss imports shouldn't see any output mentioning sass.
func sassAvailable() bool {
	_, err := exec.LookPath("sass")
	return err == nil
}

// SassAvailable is the exported probe — CLI uses it to decide
// whether to register the plugin and log the "scss support
// active via system sass" notice. Plugin registration is cheap
// when sass IS available; when it isn't, registering anyway
// means every .scss import surfaces the install-sass error,
// which is better than esbuild's silent "no loader configured"
// fallthrough.
func SassAvailable() bool { return sassAvailable() }

