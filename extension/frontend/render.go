package frontend

import (
	"fmt"

	"github.com/paulmanoni/nexus/extension"
)

// Render is the Generate driver's entrypoint. Walks the registry +
// ref pool exposed via the context and produces an []extension.File
// for the CLI (or the in-process driver) to write atomically.
//
// Output shape (all paths relative to OutDir):
//
//	_client.ts   — RPC dispatcher (transport-neutral)
//	types.ts     — interface for every named struct in the ref pool
//	index.ts     — per-op typed exports (listUsers, createUser, ...)
//	vue.ts       — per-op composables, when Framework == Vue
//	react.ts     — per-op hooks, when Framework == React
//	svelte.ts    — per-op store factories, when Framework == Svelte
//
// Each per-framework file imports the typed functions from index.ts
// and wraps them in the framework's reactive primitives. Consumers
// pick a framework once on the Plugin's Config; switching is a
// re-run away — the index.ts surface is framework-neutral and stays
// byte-identical across choices.
//
// Exported so the `nexus generate frontend` CLI can call it without
// going through the in-process driver path (the CLI runs offline
// against a manifest file or a running app's HTTP surface; no Fx
// graph in scope).
func Render(cfg Config, ctx extension.GenerateContext) ([]extension.File, error) {
	if ctx.Registry == nil {
		return nil, fmt.Errorf("frontend.Render: GenerateContext.Registry is nil")
	}

	out := make([]extension.File, 0, 4)
	out = append(out,
		extension.File{Path: "_client.ts", Body: []byte(renderClientTS(cfg, ctx))},
		extension.File{Path: "types.ts", Body: []byte(renderTypesTS(cfg, ctx))},
		extension.File{Path: "index.ts", Body: []byte(renderIndexTS(cfg, ctx))},
	)

	switch cfg.Framework {
	case Vue:
		out = append(out, extension.File{Path: "vue.ts", Body: []byte(renderVueTS(cfg, ctx))})
	case React:
		out = append(out, extension.File{Path: "react.ts", Body: []byte(renderReactTS(cfg, ctx))})
	case Svelte:
		out = append(out, extension.File{Path: "svelte.ts", Body: []byte(renderSvelteTS(cfg, ctx))})
	case None:
		// Explicit opt-out from the per-framework wrapper.
	}

	// Merge contributor output. Empty in phase 1 (the App doesn't
	// gather contributors automatically yet); kept for forward
	// compatibility so the auth/oauth2 phase-2 PR is non-breaking.
	for _, c := range ctx.Contributors {
		files, err := c.NexusContribute(ctx)
		if err != nil {
			return nil, fmt.Errorf("frontend.render: contributor failed: %w", err)
		}
		out = append(out, files...)
	}

	return out, nil
}
