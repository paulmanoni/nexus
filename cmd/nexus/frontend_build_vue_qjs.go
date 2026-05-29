//go:build !vue

// Default Vue SFC backend: CGo-free QuickJS-NG via WebAssembly
// (frontend/deps/sfc/vue/compile_qjs.go). Plain `nexus build` — no cgo,
// no build tags — compiles .vue with this, so Vue support works out of
// the box in the standard pure-Go install.
//
// Pass `-tags vue` (with CGO_ENABLED=1) to override with the native CGo
// QuickJS binding (frontend_build_vue.go), which is faster per SFC.

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
			return vue.NewQJSCompiler(bundle, "@vue/compiler-sfc@"+vue.DefaultCompilerVersion)
		})
	}
}
