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
//	react.ts     — placeholder until templates land
//	svelte.ts    — placeholder until templates land
//
// Phase 1 emits only the transport-neutral trio + Vue. React/Svelte
// gates resolve to no file (consumer reads the runtime client SDK
// in the meantime). The driver still merges ClientContributor output
// even though no contributor is wired yet — the loop costs nothing
// and lights up phase 2 without touching this function.
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
	case React, Svelte:
		// Phase-1: framework adapter templates land in a follow-up.
		// Emit nothing for now so users opting in early don't see a
		// stale stub committed into their source tree.
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
