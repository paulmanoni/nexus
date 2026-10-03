package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

// devGenerator is code generation nexus dev keeps current: it runs once
// before the first build and again when a file it watches changes. It
// writes Go into the tree, which the dev loop's watcher then rebuilds from
// like any edit.
type devGenerator struct {
	name    string
	watches func(path string) bool
	run     func() (summary string, err error) // summary "" = nothing changed
}

// devGenerators are the generators this project needs — today, views when
// the tree has .templ files. Nothing to configure.
func devGenerators(root string) []devGenerator {
	var out []devGenerator
	if viewgen.HasTemplates(root) {
		out = append(out, viewsGenerator(root))
	}
	return out
}

// viewsGenerator compiles the project's reactive templ views (package
// github.com/paulmanoni/nexus/v2/view). It writes the generated Go to disk —
// gopls reads it to resolve components across packages — and reruns on a
// .templ save, or on a Go edit that may declare what templates use (a
// state struct, a view.Shard registration).
func viewsGenerator(root string) devGenerator {
	return devGenerator{
		name: "views",
		watches: func(path string) bool {
			base := filepath.Base(path)
			if strings.HasSuffix(base, ".templ") {
				return true
			}
			generated := strings.HasSuffix(base, "_templ.go") || base == "view_gen.go" || base == "view_imports_gen.go"
			return strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go") && !generated
		},
		run: func() (string, error) {
			changed, err := viewgen.Module(root)
			if err != nil {
				// Positions are absolute; show them relative to the project.
				if abs, aerr := filepath.Abs(root); aerr == nil {
					return "", errors.New(strings.ReplaceAll(err.Error(), abs+string(filepath.Separator), ""))
				}
				return "", err
			}
			if len(changed) == 0 {
				return "", nil
			}
			return fmt.Sprintf("%d file(s) updated", len(changed)), nil
		},
	}
}

// generatorRunner runs a project's generators. Changes are debounced per
// generator, and a generator never runs twice at once: a change during a
// run schedules exactly one more.
type generatorRunner struct {
	ctx      context.Context
	gens     []devGenerator
	out      io.Writer
	debounce time.Duration

	mu      sync.Mutex
	timers  map[int]*time.Timer
	running map[int]bool
	again   map[int]bool
	wg      sync.WaitGroup
}

func newGeneratorRunner(ctx context.Context, gens []devGenerator, out io.Writer) *generatorRunner {
	return &generatorRunner{
		ctx: ctx, gens: gens, out: out, debounce: 100 * time.Millisecond,
		timers: map[int]*time.Timer{}, running: map[int]bool{}, again: map[int]bool{},
	}
}

// runAll runs every generator once, in order, and waits — before the first
// build, so it compiles the generated code.
func (r *generatorRunner) runAll() {
	for i := range r.gens {
		r.run(i)
	}
}

// changed tells the runner a file changed; generators watching it run
// after the debounce.
func (r *generatorRunner) changed(path string) {
	for i, g := range r.gens {
		if !g.watches(path) {
			continue
		}
		r.mu.Lock()
		if t := r.timers[i]; t != nil {
			t.Stop()
		}
		r.timers[i] = time.AfterFunc(r.debounce, func() { r.start(i) })
		r.mu.Unlock()
	}
}

func (r *generatorRunner) start(i int) {
	r.mu.Lock()
	if r.running[i] {
		r.again[i] = true
		r.mu.Unlock()
		return
	}
	r.running[i] = true
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		for {
			r.run(i)
			r.mu.Lock()
			if !r.again[i] || r.ctx.Err() != nil {
				r.running[i] = false
				r.mu.Unlock()
				return
			}
			r.again[i] = false
			r.mu.Unlock()
		}
	}()
}

// wait blocks until no generator is running.
func (r *generatorRunner) wait() { r.wg.Wait() }

// run executes generator i and reports what it changed. A failure is
// printed line by line (positions point into the source); the dev loop
// keeps serving the last good build.
func (r *generatorRunner) run(i int) {
	g := r.gens[i]
	if r.ctx.Err() != nil {
		return
	}
	begin := time.Now()
	summary, err := g.run()
	elapsed := time.Since(begin).Round(time.Millisecond)
	if err != nil {
		fmt.Fprintf(r.out, "  %s●%s %s failed (%s):\n", ansiRed, ansiReset, g.name, elapsed)
		for _, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
			fmt.Fprintf(r.out, "    %s[%s]%s %s\n", ansiRed, g.name, ansiReset, line)
		}
		return
	}
	if summary != "" {
		fmt.Fprintf(r.out, "  %s● %s · %s (%s)%s\n", ansiDim, g.name, summary, elapsed, ansiReset)
	}
}
