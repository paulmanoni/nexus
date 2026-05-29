package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// bootstrapVuePlugin is the backend-agnostic Vue SFC wiring shared by
// the default WASM hook (frontend_build_vue_qjs.go) and the opt-in CGo
// hook (frontend_build_vue.go). It bootstraps the @vue/compiler-sfc
// bundle once (cached after the first call), builds a pool of
// newCompiler instances sized to the machine, wires the esbuild
// plugin, and returns it plus a teardown.
//
// The 60s timeout covers a cold first build (fetch + esbuild bundle of
// @vue/compiler-sfc takes 1-5s typically); warm calls hit the cache and
// return in milliseconds.
func bootstrapVuePlugin(st *store.Store, newCompiler func(bundle []byte) (vue.SFCCompiler, error)) (func(), api.Plugin, error) {
	registry := os.Getenv("NEXUS_REGISTRY")
	if registry == "" {
		registry = fetcher.DefaultRegistry
	}
	f := fetcher.New(st, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bundle, err := vue.Bootstrap(ctx, vue.BootstrapOptions{Store: st, Fetcher: f})
	if err != nil {
		return nil, api.Plugin{}, fmt.Errorf("bootstrap vue compiler: %w", err)
	}

	// Pool the compilers so .vue files compile concurrently: esbuild
	// fans OnLoad callbacks across its worker goroutines, but each
	// compiler serializes internally, so a single one would funnel a
	// whole Vue app's SFCs through one interpreter.
	pool, err := vue.NewPool(func() (vue.SFCCompiler, error) { return newCompiler(bundle) }, vuePoolSize())
	if err != nil {
		return nil, api.Plugin{}, fmt.Errorf("init vue compiler: %w", err)
	}
	plugin, err := vue.Plugin(pool)
	if err != nil {
		pool.Close()
		return nil, api.Plugin{}, fmt.Errorf("wire vue plugin: %w", err)
	}
	return func() { pool.Close() }, plugin, nil
}

// vuePoolSize picks how many compilers to run in parallel. Defaults to
// the CPU count, capped at 8 — beyond that the build is almost always
// bottlenecked on esbuild/IO rather than SFC compile, and each compiler
// holds its own copy of the ~850 KB compiler bundle (the WASM backend
// also a multi-MB Wazero instance), so an unbounded pool on a many-core
// box wastes RAM for no speedup.
//
// NEXUS_VUE_POOL overrides the default for tuning; any value < 1 (or
// unparseable) falls back to the computed default.
func vuePoolSize() int {
	n := max(min(runtime.GOMAXPROCS(0), 8), 1)
	if v := os.Getenv("NEXUS_VUE_POOL"); v != "" {
		if p, perr := strconv.Atoi(v); perr == nil && p >= 1 {
			n = p
		}
	}
	return n
}
