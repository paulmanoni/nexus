// Package vue compiles single-file Vue components (.vue) into
// plain ES modules at bundle time, without any Node.js. The
// compilation happens entirely in-process: a Goja JavaScript
// runtime hosts the @vue/compiler-sfc package and we marshal
// source bytes in / generated code out via a thin adapter API.
//
// Lifecycle:
//
//  1. The user runs `nexus add vue` once per project. The
//     fetcher transitively pulls @vue/compiler-sfc + its deps
//     into ~/.nexus/cache.
//  2. On the first .vue compile in a project, the bundler asks
//     the Vue plugin to load. The plugin reads the compiler
//     bundle from disk (built once via esbuild during install)
//     and constructs a Compiler.
//  3. Every .vue file the bundler encounters routes through
//     Compiler.Compile, which invokes the in-Goja
//     __nexus_compileSFC adapter and returns the resulting JS.
//
// The Compiler is goroutine-safe via an internal mutex: Goja
// runtimes are NOT thread-safe (a single runtime instance must
// not be shared across goroutines without serialization). For a
// build loop that processes .vue files in parallel, callers can
// either rely on the mutex (simple, slower) or instantiate one
// Compiler per worker (faster, more memory). v0.1 takes the
// simple path; profiling can drive the change if SFC compile
// becomes a hot spot.
package vue

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

// Compiler holds a long-lived Goja runtime preloaded with the
// @vue/compiler-sfc bundle. One Compiler typically lives for the
// duration of a single `nexus build` or `nexus dev` session.
type Compiler struct {
	runtime *goja.Runtime
	compile goja.Callable
	version string

	mu sync.Mutex
}

// CompileResult is what Compiler.Compile returns: the synthesized
// JS module plus any diagnostics the compiler produced. A non-nil
// Errors slice means the result is partially valid (esbuild may
// or may not be able to bundle it) — callers typically surface
// the errors and abort the build before reaching esbuild.
type CompileResult struct {
	Code   string
	Errors []CompileError
}

// CompileError describes a single diagnostic from the SFC
// compiler. Line/Column are 1-indexed if known, else zero.
type CompileError struct {
	Message string
	Line    int
	Column  int
}

// NewCompiler constructs a Compiler from the supplied bundle
// bytes. The bundle MUST evaluate at top level to install a
// global function named __nexus_compileSFC; see the adapter
// contract in testdata/fake-adapter.js for the exact shape.
//
// version is a free-form identifier (typically "vue-compiler@<n>")
// used only for diagnostics + the `nexus install` cache key.
func NewCompiler(bundle []byte, version string) (*Compiler, error) {
	if len(bundle) == 0 {
		return nil, errors.New("vue: empty compiler bundle")
	}
	rt := goja.New()
	// Install minimal host polyfills BEFORE evaluating the bundle.
	// @vue/compiler-sfc (and its dep chain) routinely call
	// console.warn / console.log when it encounters edge cases;
	// without these, the bundle blows up at module-evaluation
	// time. process.env.NODE_ENV is mostly handled by esbuild's
	// Define at bundle time, but a runtime fallback covers the
	// `typeof process` branches Define can't substitute.
	if err := installPolyfills(rt); err != nil {
		return nil, fmt.Errorf("vue: install polyfills: %w", err)
	}
	// Goja gotcha: top-level "var" decls produce globals;
	// top-level "let/const" do NOT. The adapter must explicitly
	// assign to globalThis. Documented in the adapter contract.
	if _, err := rt.RunString(string(bundle)); err != nil {
		return nil, fmt.Errorf("vue: load bundle: %w", err)
	}
	fn, ok := goja.AssertFunction(rt.GlobalObject().Get("__nexus_compileSFC"))
	if !ok {
		return nil, errors.New("vue: bundle did not expose globalThis.__nexus_compileSFC after load")
	}
	return &Compiler{
		runtime: rt,
		compile: fn,
		version: version,
	}, nil
}

// Compile runs the bundled @vue/compiler-sfc against source. The
// returned CompileResult.Code is JS ready to feed back into esbuild
// (treated as a regular module). Errors carries any diagnostics
// the compiler produced; a non-empty slice typically means the
// caller should surface them and abort the build.
//
// Takes the mutex for the call — Goja runtimes can NOT have
// multiple goroutines calling into them concurrently. Documented
// as such in goja's README.
func (c *Compiler) Compile(source, filename string) (CompileResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	resultValue, err := c.compile(
		goja.Undefined(),
		c.runtime.ToValue(source),
		c.runtime.ToValue(filename),
	)
	if err != nil {
		return CompileResult{}, fmt.Errorf("vue: __nexus_compileSFC threw: %w", err)
	}
	return decodeResult(c.runtime, resultValue)
}

// Version returns the identifier passed to NewCompiler — useful in
// diagnostics ("compiled with vue-compiler@3.4.21") and for cache
// invalidation logic in the install command.
func (c *Compiler) Version() string { return c.version }

// installPolyfills sets minimum globals on the runtime so a real
// @vue/compiler-sfc bundle can evaluate without crashing on
// undefined host APIs. The set is intentionally tiny — we add to
// it only when a real-world Vue compile blows up against it.
//
// Goja: ES5.1 + most non-async ES6 (lambdas, classes, generators
// via stdgen, let/const, destructuring, template literals, Map,
// Set, Promise, Symbol). Missing: async generators (esbuild
// down-levels them via Target=es2015), top-level await,
// Node built-ins (we serve those via esm.sh's polyfills which
// the fetcher pulls in).
func installPolyfills(rt *goja.Runtime) error {
	// console — Vue's compiler logs deprecation warnings via it.
	consoleSrc := `globalThis.console = globalThis.console || {
		log:   function(){},
		info:  function(){},
		warn:  function(){},
		error: function(){},
		debug: function(){},
		trace: function(){},
	};`
	if _, err := rt.RunString(consoleSrc); err != nil {
		return err
	}
	// process.env.NODE_ENV — esbuild's Define handles most call
	// sites at bundle time but a runtime fallback covers the
	// `if (typeof process !== "undefined")` branches that survive
	// Define unchanged.
	processSrc := `globalThis.process = globalThis.process || {
		env: { NODE_ENV: "production" },
		platform: "linux",
		versions: { node: "0.0.0" },
	};`
	if _, err := rt.RunString(processSrc); err != nil {
		return err
	}
	// globalThis itself — Goja 0.x defines it but older builds
	// don't; harmless to assert it as a self-reference. Same for
	// self and global, both aliases used by some libraries.
	aliasSrc := `if (typeof self === "undefined") globalThis.self = globalThis;
		if (typeof global === "undefined") globalThis.global = globalThis;`
	if _, err := rt.RunString(aliasSrc); err != nil {
		return err
	}
	// btoa / atob — browser base64 APIs. esm.sh's unenv polyfill
	// for Node's Buffer module does `var Xt=globalThis.btoa.bind(globalThis)`
	// at top level; without btoa, the `.bind` deref crashes
	// module evaluation. Wire Go's encoding/base64 through.
	rt.Set("btoa", func(s string) string {
		return base64.StdEncoding.EncodeToString([]byte(s))
	})
	rt.Set("atob", func(s string) (string, error) {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return "", err
		}
		return string(b), nil
	})
	return nil
}

// decodeResult walks the JS object the adapter returned and pulls
// out the {code, errors} fields into a Go-side CompileResult.
//
// The shape we expect (and the shape the production
// @vue/compiler-sfc adapter is contractually obligated to emit):
//
//	{ code: string, errors?: [{message, line?, column?}] }
//
// Missing fields are tolerated — code falls back to "" and errors
// to nil. Extra fields are silently ignored. This keeps the
// Go ↔ JS contract loose enough that we can extend the adapter
// without versioning the Go side.
func decodeResult(rt *goja.Runtime, v goja.Value) (CompileResult, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return CompileResult{}, errors.New("vue: __nexus_compileSFC returned null/undefined")
	}
	obj := v.ToObject(rt)
	if obj == nil {
		return CompileResult{}, fmt.Errorf("vue: __nexus_compileSFC returned non-object %T", v.Export())
	}

	var res CompileResult
	if code := obj.Get("code"); code != nil && !goja.IsUndefined(code) {
		res.Code = code.String()
	}
	if errsVal := obj.Get("errors"); errsVal != nil && !goja.IsUndefined(errsVal) && !goja.IsNull(errsVal) {
		errsObj := errsVal.ToObject(rt)
		length := int(errsObj.Get("length").ToInteger())
		for i := 0; i < length; i++ {
			itemVal := errsObj.Get(fmt.Sprintf("%d", i))
			if itemVal == nil || goja.IsUndefined(itemVal) {
				continue
			}
			item := itemVal.ToObject(rt)
			ce := CompileError{}
			if m := item.Get("message"); m != nil && !goja.IsUndefined(m) {
				ce.Message = m.String()
			}
			if l := item.Get("line"); l != nil && !goja.IsUndefined(l) && !goja.IsNull(l) {
				ce.Line = int(l.ToInteger())
			}
			if cc := item.Get("column"); cc != nil && !goja.IsUndefined(cc) && !goja.IsNull(cc) {
				ce.Column = int(cc.ToInteger())
			}
			res.Errors = append(res.Errors, ce)
		}
	}
	return res, nil
}
