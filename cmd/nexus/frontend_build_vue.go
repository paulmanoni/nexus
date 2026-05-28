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
	"runtime"
	"strconv"
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
	// Build a pool of compilers rather than a single one so .vue
	// files compile concurrently. esbuild fans OnLoad callbacks out
	// across its worker goroutines, but each QuickJS compiler
	// serializes internally — one compiler would funnel a whole
	// Vue app's SFCs through a single interpreter. The pool lets up
	// to vuePoolSize() of them run at once.
	pool, err := vue.NewPool(bundle, "@vue/compiler-sfc@"+vue.DefaultCompilerVersion, vuePoolSize())
	if err != nil {
		return nil, api.Plugin{}, fmt.Errorf("init vue compiler: %w", err)
	}
	plugin, err := vue.Plugin(pool)
	if err != nil {
		pool.Close()
		return nil, api.Plugin{}, fmt.Errorf("wire vue plugin: %w", err)
	}
	teardown := func() { pool.Close() }
	_ = lf // currently unused — kept in the signature for future
	// per-project compiler-version pinning that reads lf.
	return teardown, plugin, nil
}

// vuePoolSize picks how many QuickJS compilers to run in parallel.
// Defaults to the CPU count, capped at 8 — beyond that the build is
// almost always bottlenecked on esbuild/IO rather than SFC compile,
// and each compiler holds its own ~850 KB bundle parse in memory, so
// an unbounded pool on a many-core box wastes RAM for no speedup.
//
// NEXUS_VUE_POOL overrides the default for tuning/debugging; any
// value < 1 (or unparseable) falls back to the computed default.
func vuePoolSize() int {
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	if v := os.Getenv("NEXUS_VUE_POOL"); v != "" {
		if p, perr := strconv.Atoi(v); perr == nil && p >= 1 {
			n = p
		}
	}
	return n
}
