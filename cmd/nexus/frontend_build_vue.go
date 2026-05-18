//go:build cgo && vue

// This file is the cgo+vue-only registration point that wires the
// Vue SFC compiler into the frontend build pipeline. Default
// builds (no cgo, no vue tag) skip it entirely; `nexus build`
// then rejects .vue with a clear message because vueCompilerHook
// stays nil.
//
// Opt in with:
//
//	CGO_ENABLED=1 go install -tags vue github.com/paulmanoni/nexus/cmd/nexus@latest
//
// The QuickJS-backed compiler this hook depends on lives in
// frontend/deps/sfc/vue/ and is itself build-tagged //go:build cgo
// — it only compiles when CGo is on, and only loads here when the
// vue tag is also passed. Two-tag gate keeps the pure-Go default
// install unaffected.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// init populates the hook frontend_build.go reads. When this file
// isn't compiled (no cgo+vue tag), the hook stays nil and .vue
// gets rejected with a clear "rebuild with -tags vue" message.
func init() {
	vueCompilerHook = bootstrapAndPlugin
}

// bootstrapAndPlugin is the one-shot setup: bootstrap the
// compiler bundle (cached at ~/.nexus/cache/sfc-vue/<version>/
// compiler.bundle.js after the first call), open a QuickJS
// runtime around it, return the SFC plugin + a teardown.
//
// The 60s timeout covers a cold first build (fetch + esbuild
// bundle of @vue/compiler-sfc takes 1-5s typically); warm calls
// hit the cache and return in milliseconds.
func bootstrapAndPlugin(lf *lockfile.File, st *store.Store) (func(), api.Plugin, error) {
	registry := os.Getenv("NEXUS_REGISTRY")
	if registry == "" {
		registry = fetcher.DefaultRegistry
	}
	f := fetcher.New(st, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bundle, err := vue.Bootstrap(ctx, vue.BootstrapOptions{
		Store:   st,
		Fetcher: f,
	})
	if err != nil {
		return nil, api.Plugin{}, fmt.Errorf("bootstrap vue compiler: %w", err)
	}
	compiler, err := vue.NewCompiler(bundle, "@vue/compiler-sfc@"+vue.DefaultCompilerVersion)
	if err != nil {
		return nil, api.Plugin{}, fmt.Errorf("init vue compiler: %w", err)
	}
	plugin, err := vue.Plugin(compiler)
	if err != nil {
		compiler.Close()
		return nil, api.Plugin{}, fmt.Errorf("wire vue plugin: %w", err)
	}
	teardown := func() { compiler.Close() }
	_ = lf // currently unused — kept in the signature for future
	// per-project compiler-version pinning that reads lf.
	return teardown, plugin, nil
}
