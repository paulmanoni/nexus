//go:build cgo && vue

// Opt-in native Vue SFC backend: the CGo binding to QuickJS (compiled
// in frontend/deps/sfc/vue/compile.go, which is //go:build cgo). Build
// with:
//
//	CGO_ENABLED=1 go install -tags vue github.com/paulmanoni/nexus/cmd/nexus@latest
//
// Without `-tags vue` the default WASM backend (frontend_build_vue_qjs.go)
// is used instead — that one needs no cgo. The native binding is faster
// per SFC but requires a C toolchain.

package main

import (
	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

func init() {
	vueCompilerHook = func(_ *lockfile.File, st *store.Store) (func(), api.Plugin, error) {
		return bootstrapVuePlugin(st, func(bundle []byte) (vue.SFCCompiler, error) {
			return vue.NewCompiler(bundle, "@vue/compiler-sfc@"+vue.DefaultCompilerVersion)
		})
	}
}
