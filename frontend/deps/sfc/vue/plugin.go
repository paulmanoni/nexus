//go:build cgo
// +build cgo

package vue

import (
	"errors"
	"fmt"
	"os"

	"github.com/evanw/esbuild/pkg/api"
)

// Plugin wraps a Compiler in an esbuild plugin so the bundler
// picks up .vue files transparently. Register with
// Bundler.AddPlugin AFTER the resolver plugin (resolver handles
// bare imports; this one handles .vue file loads).
//
// Compile errors surface as esbuild Messages, which causes the
// build to fail loudly with the file + line + col + message. We
// do NOT emit any code when the compiler reports errors — better
// to fail than to feed esbuild half-compiled JS that produces a
// second cascade of errors.
func Plugin(c *Compiler) (api.Plugin, error) {
	if c == nil {
		return api.Plugin{}, errors.New("vue: Plugin called with nil Compiler")
	}
	return api.Plugin{
		Name: "nexus-vue-sfc",
		Setup: func(build api.PluginBuild) {
			build.OnLoad(api.OnLoadOptions{Filter: `\.vue$`}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				source, err := os.ReadFile(args.Path)
				if err != nil {
					return api.OnLoadResult{}, fmt.Errorf("vue: read %s: %w", args.Path, err)
				}
				res, err := c.Compile(string(source), args.Path)
				if err != nil {
					return api.OnLoadResult{
						Errors: []api.Message{{
							Text:     err.Error(),
							Location: &api.Location{File: args.Path},
						}},
					}, nil
				}
				if len(res.Errors) > 0 {
					msgs := make([]api.Message, 0, len(res.Errors))
					for _, ce := range res.Errors {
						msgs = append(msgs, api.Message{
							Text: ce.Message,
							Location: &api.Location{
								File:   args.Path,
								Line:   ce.Line,
								Column: ce.Column,
							},
						})
					}
					return api.OnLoadResult{Errors: msgs}, nil
				}
				contents := res.Code
				return api.OnLoadResult{
					Contents: &contents,
					Loader:   api.LoaderJS,
				}, nil
			})
		},
	}, nil
}
