//go:build cgo
// +build cgo

package vue

import (
	"fmt"
	"sync"
)

// Pool is a fixed-size set of Compilers that compiles .vue files
// concurrently. A single Compiler serializes every Compile call on
// its one QuickJS worker thread (see compile.go), so a Vue-heavy
// build — where esbuild fans out OnLoad callbacks across all its
// worker goroutines — bottlenecks on that one interpreter. A Pool
// removes the bottleneck by holding N independent QuickJS contexts
// and handing each incoming Compile to a free one, bounding
// concurrency at the pool size.
//
// Pool satisfies the same SFCCompiler interface as *Compiler, so
// Plugin accepts either: callers that don't care about throughput
// (tests, single-file tools) pass a lone *Compiler; the build
// pipeline passes a Pool sized to the machine.
//
// Memory cost scales linearly: each compiler parses the ~850 KB
// @vue/compiler-sfc bundle into its own runtime, so a pool of N
// holds N copies. Callers pick N as a CPU/memory trade-off — see
// the CLI's sizing helper.
type Pool struct {
	version   string
	compilers []*Compiler
	// idle is a buffered channel used as a free-list. It's pre-
	// filled with every compiler at construction; Compile takes one
	// and returns it when done. Capacity equals the compiler count,
	// so the return send never blocks. We deliberately never close
	// it — after Close the buffered compilers are still drainable,
	// and Compile on a closed compiler returns a clean error rather
	// than panicking on a send-to-closed-channel.
	idle chan *Compiler
	// closed is closed by Close before the underlying compilers are
	// torn down. Compile checks it first so a call after Close
	// returns a clean error instead of pulling a now-dead compiler
	// off the free-list.
	closed    chan struct{}
	closeOnce sync.Once
}

// NewPool builds size Compilers from the same bundle bytes and
// returns a Pool that load-balances across them. size < 1 is
// clamped to 1, making a Pool behave exactly like a single
// Compiler.
//
// Compilers are constructed concurrently: each NewCompiler blocks
// ~100 ms parsing the bundle, so building them in parallel keeps
// pool startup at roughly one compiler's cost instead of N times
// it. If any compiler fails to bootstrap, every already-built one
// is closed and the first error is returned — a half-built pool is
// never handed back.
func NewPool(bundle []byte, version string, size int) (*Pool, error) {
	if len(bundle) == 0 {
		return nil, fmt.Errorf("vue: empty compiler bundle")
	}
	if size < 1 {
		size = 1
	}

	p := &Pool{
		version: version,
		idle:    make(chan *Compiler, size),
		closed:  make(chan struct{}),
	}

	type built struct {
		c   *Compiler
		err error
	}
	results := make(chan built, size)
	for i := 0; i < size; i++ {
		go func() {
			c, err := NewCompiler(bundle, version)
			results <- built{c: c, err: err}
		}()
	}

	var firstErr error
	for i := 0; i < size; i++ {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		p.compilers = append(p.compilers, r.c)
		p.idle <- r.c
	}
	if firstErr != nil {
		p.Close() // tear down any that did come up
		return nil, fmt.Errorf("vue: build compiler pool: %w", firstErr)
	}
	return p, nil
}

// Compile checks out a free compiler, runs the SFC compile on it,
// and returns it to the pool. Blocks while every compiler is busy,
// which is the intended backpressure: in-flight compiles never
// exceed the pool size. Safe for concurrent use from esbuild's
// OnLoad fan-out.
func (p *Pool) Compile(source, filename string) (CompileResult, error) {
	// Non-blocking shutdown check first. Both p.closed and p.idle
	// can be ready at once after Close (idle stays buffered), and a
	// plain select would pick a dead compiler half the time — so we
	// short-circuit on closed before ever touching the free-list.
	select {
	case <-p.closed:
		return CompileResult{}, fmt.Errorf("vue: Compile on closed pool")
	default:
	}
	c := <-p.idle
	defer func() { p.idle <- c }()
	return c.Compile(source, filename)
}

// Close shuts down every compiler in the pool and releases their
// QuickJS runtimes. Idempotent. Callers MUST call it when done —
// nothing else frees the C-side state.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.closed) // signal Compile before tearing compilers down
		for _, c := range p.compilers {
			c.Close()
		}
	})
}

// Version returns the identifier the pool's compilers were built
// with. Matches Compiler.Version.
func (p *Pool) Version() string { return p.version }

// Size reports how many compilers the pool holds — i.e. its maximum
// compile concurrency.
func (p *Pool) Size() int { return len(p.compilers) }
