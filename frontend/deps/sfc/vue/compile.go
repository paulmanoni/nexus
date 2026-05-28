//go:build cgo
// +build cgo

// Package vue compiles single-file Vue components (.vue) into
// plain ES modules at bundle time without Node.js. The
// compilation runs in-process via a QuickJS interpreter hosting
// the real @vue/compiler-sfc package.
//
// Build constraint: this package only compiles with CGo enabled
// because buke/quickjs-go binds the QuickJS C library through
// CGo. Default `go build` against the framework skips this
// package entirely; users who want .vue compile opt in with
// CGO_ENABLED=1 go install -tags vue ./cmd/nexus@latest.
// The frontend_build.go layer detects whether the Vue plugin
// is registered (via an init() in a sibling cgo-tagged file)
// and either uses it or surfaces a clear "no Vue support in
// this build" message — keeping the pure-Go static-binary
// story intact for users who don't need .vue.
//
// Why QuickJS rather than Goja: @vue/compiler-sfc's transitive
// dependency on @babel/parser does runtime feature detection
// by introspecting an async-generator function's prototype
// chain. Goja can't run async generators at all, and
// esm.sh's `?target=es2015` lowering rewrote the syntax
// (`async function*` → regular function) without rewriting
// the introspection, breaking the detection at module
// evaluation time. QuickJS supports async generators natively,
// so the unmodified expression evaluates to the real
// AsyncIterator.prototype and the detection succeeds.
//
// Lifecycle:
//
//  1. The user runs `nexus add vue` once per project to pull
//     vue + its compile-time deps into ~/.nexus/cache.
//  2. On the first .vue compile, Bootstrap (see bootstrap.go)
//     fetches @vue/compiler-sfc, esbuild-bundles it with the
//     adapter, caches the result.
//  3. NewCompiler loads the cached bundle into a fresh QuickJS
//     context. The adapter's IIFE installs
//     globalThis.__nexus_compileSFC.
//  4. Every .vue file routes through Compile, which calls
//     __nexus_compileSFC and returns the generated JS.
//
// The Compiler is goroutine-safe via an internal mutex: a
// QuickJS context isn't safe for concurrent use across threads.
// For a parallel build loop, callers can either rely on the
// mutex (simple, slower) or instantiate one Compiler per
// worker (faster, more memory). v0.1 takes the simple path.
package vue

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/buke/quickjs-go"
)

// Compiler is a façade over a QuickJS runtime + context pinned to
// a single OS thread.
//
// Why the dedicated worker goroutine: QuickJS has thread-affinity
// — a runtime/context pair must only be touched from the OS
// thread that created it, or the underlying C state corrupts.
// Go's goroutine scheduler is free to move goroutines between OS
// threads at preemption points, so even a perfectly serialized
// mutex on Compile() isn't enough; the SECOND caller might end up
// on a different OS thread than the FIRST. We solve this with the
// canonical pattern: one worker goroutine doing `runtime.
// LockOSThread()` at startup, fed via a buffered job channel.
// Compile() sends a job and blocks on the reply channel until the
// worker finishes.
//
// One Compiler typically lives for a `nexus build` or `nexus dev`
// session. Cheap to construct from a cached bundle (~100ms once
// the QuickJS runtime parses 850 KB).
type Compiler struct {
	version string

	jobs   chan compileJob
	closed chan struct{}
	once   sync.Once
}

// compileJob is one in-flight Compile call. reply MUST be a
// buffered channel of size 1 — the worker writes one result and
// moves on; an unbuffered channel would block the worker if the
// caller went away (timeout, ctx cancel).
type compileJob struct {
	source, filename string
	reply            chan compileReply
}

type compileReply struct {
	result CompileResult
	err    error
}

// NewCompiler constructs a Compiler from the supplied bundle bytes.
// The bundle MUST evaluate at top level to install a global
// function named __nexus_compileSFC; see the adapter contract in
// adapter.js for the exact shape.
//
// version is a free-form identifier (typically
// "@vue/compiler-sfc@3.4.21") used only for diagnostics + the
// `nexus install` cache key.
//
// Spawns the worker goroutine before returning. If bundle
// evaluation fails on the worker, the error is propagated back
// here synchronously and the worker exits cleanly so the caller
// doesn't leak.
func NewCompiler(bundle []byte, version string) (*Compiler, error) {
	if len(bundle) == 0 {
		return nil, errors.New("vue: empty compiler bundle")
	}

	c := &Compiler{
		version: version,
		jobs:    make(chan compileJob),
		closed:  make(chan struct{}),
	}
	// Synchronous bootstrap: the worker reports load-time success
	// or failure via initErr before accepting any jobs. We block
	// here until it's ready, so NewCompiler's return value is
	// only ever "ready to Compile" or "error, caller retries".
	initErr := make(chan error, 1)
	go c.worker(bundle, initErr)
	if err := <-initErr; err != nil {
		// Worker has already cleaned up its QuickJS resources +
		// will exit on its own; we just drop our handle.
		return nil, err
	}
	return c, nil
}

// Close shuts down the worker goroutine and releases QuickJS
// resources. Idempotent — second + subsequent calls are no-ops.
// Callers MUST call this when done; nothing else will release
// the C-side runtime.
func (c *Compiler) Close() {
	c.once.Do(func() {
		close(c.jobs)
		<-c.closed
	})
}

// Compile runs the bundled @vue/compiler-sfc against source.
// Returns the synthesized JS module + any diagnostics. Safe for
// concurrent use; calls are serialized at the worker via the jobs
// channel.
func (c *Compiler) Compile(source, filename string) (CompileResult, error) {
	// Check for shutdown BEFORE attempting the send. Close() closes
	// c.jobs, and a send on a closed channel panics — in a select,
	// the send case is "ready" (it would proceed and panic), so a
	// plain select between the send and <-c.closed picks the panic
	// path ~half the time once both are closed. This non-blocking
	// guard returns the documented error first when we've already
	// been closed.
	select {
	case <-c.closed:
		return CompileResult{}, errors.New("vue: Compile called after Close")
	default:
	}
	reply := make(chan compileReply, 1)
	select {
	case c.jobs <- compileJob{source: source, filename: filename, reply: reply}:
	case <-c.closed:
		return CompileResult{}, errors.New("vue: Compile called after Close")
	}
	r := <-reply
	return r.result, r.err
}

// worker owns the QuickJS runtime + context. Pinned to one OS
// thread via runtime.LockOSThread so QuickJS's C-side state is
// only ever touched from the thread that created it.
//
// initErr fires once during bundle load; nil means "ready to
// accept jobs", non-nil means "the worker is dying, caller should
// give up". Either way the worker eventually closes c.closed
// before returning, so Close() can wait on it.
func (c *Compiler) worker(bundle []byte, initErr chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(c.closed)

	rt := quickjs.NewRuntime()
	defer rt.Close()
	ctx := rt.NewContext()
	defer ctx.Close()

	// Install host polyfills the production Vue compiler bundle
	// expects from a browser-shaped environment but QuickJS
	// doesn't ship by default. esm.sh's unenv buffer polyfill in
	// particular does `globalThis.btoa.bind(globalThis)` at top
	// level, so missing btoa = bundle dies on the first `.bind`
	// deref.
	if err := installPolyfills(ctx); err != nil {
		initErr <- fmt.Errorf("vue: install polyfills: %w", err)
		return
	}

	// Evaluate the bundle. If it throws, surface the JS exception
	// as a Go error and bail.
	v := ctx.Eval(string(bundle), quickjs.EvalFileName("nexus-vue-compiler.bundle.js"))
	if v.IsException() {
		err := ctx.Exception()
		v.Free()
		initErr <- fmt.Errorf("vue: load bundle: %w", err)
		return
	}
	v.Free()

	// Sanity-check the adapter installed the expected global.
	probe := ctx.Eval(`typeof globalThis.__nexus_compileSFC`)
	ok := probe.String() == "function"
	probe.Free()
	if !ok {
		initErr <- errors.New("vue: bundle did not expose globalThis.__nexus_compileSFC after load")
		return
	}

	// Bootstrap succeeded — release the caller.
	initErr <- nil

	for job := range c.jobs {
		job.reply <- doCompile(ctx, job.source, job.filename)
	}
}

// installPolyfills wires the minimum host globals the production
// @vue/compiler-sfc bundle expects but QuickJS doesn't ship.
// Kept tight on purpose — every addition is one more thing we own
// the spec compliance of. Bumped when a real-world compile hits
// a new gap.
//
// Currently covered:
//
//	btoa / atob              esm.sh's unenv polyfill calls
//	                         globalThis.btoa.bind at top level
//	process.env.NODE_ENV     esbuild's Define handles most call
//	                         sites at bundle time; a runtime
//	                         fallback covers `typeof process`
//	                         branches Define can't substitute
//	self / global aliases    some bundles assume one of these is
//	                         a globalThis alias
//
// Console is already wired by QuickJS's standard intrinsics —
// we don't need to define it ourselves.
func installPolyfills(ctx *quickjs.Context) error {
	// Register btoa / atob as Go functions on globalThis.
	// QuickJS's binding uses base64.StdEncoding which matches
	// the WHATWG spec for these names.
	globals := ctx.Globals()

	btoaFn := ctx.Function(func(_ *quickjs.Context, _ *quickjs.Value, args []*quickjs.Value) *quickjs.Value {
		if len(args) == 0 {
			return ctx.NewString("")
		}
		return ctx.NewString(base64.StdEncoding.EncodeToString([]byte(args[0].String())))
	})
	globals.Set("btoa", btoaFn)

	atobFn := ctx.Function(func(_ *quickjs.Context, _ *quickjs.Value, args []*quickjs.Value) *quickjs.Value {
		if len(args) == 0 {
			return ctx.NewString("")
		}
		b, err := base64.StdEncoding.DecodeString(args[0].String())
		if err != nil {
			return ctx.ThrowError(fmt.Errorf("atob: invalid base64: %w", err))
		}
		return ctx.NewString(string(b))
	})
	globals.Set("atob", atobFn)

	// process.env.NODE_ENV + self / global aliases via a small
	// JS prelude — easier than building object literals through
	// the binding's API.
	prelude := `
		(function () {
			if (typeof globalThis.process === 'undefined') {
				globalThis.process = { env: { NODE_ENV: 'production' } };
			} else if (!globalThis.process.env) {
				globalThis.process.env = { NODE_ENV: 'production' };
			}
			if (typeof globalThis.self === 'undefined') {
				globalThis.self = globalThis;
			}
			if (typeof globalThis.global === 'undefined') {
				globalThis.global = globalThis;
			}
		})();
	`
	r := ctx.Eval(prelude, quickjs.EvalFileName("nexus-vue-polyfills.js"))
	defer r.Free()
	if r.IsException() {
		return fmt.Errorf("polyfill prelude: %w", ctx.Exception())
	}
	return nil
}

// doCompile runs one Compile inside the worker's OS thread.
// Extracted as a non-method so it can be unit-tested separately
// from the worker plumbing if needed.
//
// Inline both args as JSON literals so the call is one Eval with
// no Globals().Set lifecycle gotchas. JSON-encoded strings are
// valid JS expressions, handle every escape / unicode case
// correctly, and never collide with QuickJS's refcount on staged
// Values.
func doCompile(ctx *quickjs.Context, source, filename string) compileReply {
	srcJSON, _ := json.Marshal(source)
	fileJSON, _ := json.Marshal(filename)
	expr := `JSON.stringify(__nexus_compileSFC(` + string(srcJSON) + `,` + string(fileJSON) + `))`
	v := ctx.Eval(expr, quickjs.EvalFileName("nexus-vue-compile-call.js"))
	defer v.Free()
	if v.IsException() {
		return compileReply{err: fmt.Errorf("vue: __nexus_compileSFC threw: %w", ctx.Exception())}
	}
	res, err := decodeResult(v.String())
	return compileReply{result: res, err: err}
}

// Version returns the identifier passed to NewCompiler — useful in
// diagnostics ("compiled with vue-compiler@3.4.21") and for cache
// invalidation logic in the install command.
func (c *Compiler) Version() string { return c.version }

