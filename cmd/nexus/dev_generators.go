package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
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
	// notes are the warnings of the last run, when they changed since the
	// ones shown before (nil: nothing new to show).
	notes func() []string
}

// warningNotes keeps the last warnings shown, to show them again only when
// they change.
type warningNotes struct {
	last, next string
	lines      []string
}

func (n *warningNotes) set(root string, ws []*viewgen.PositionError) {
	n.lines = n.lines[:0]
	for _, w := range ws {
		n.lines = append(n.lines, viewsError(root, w).Error())
	}
	n.next = strings.Join(n.lines, "\n")
}

func (n *warningNotes) take() []string {
	if n.next == n.last {
		return nil
	}
	n.last = n.next
	if len(n.lines) == 0 {
		return []string{"warnings resolved"}
	}
	return append([]string(nil), n.lines...)
}

// devGenerators are the generators this project needs — today, views when
// the tree has .templ files. Nothing to configure. With a rebuild func the
// views compile in memory (the dev build overlays them) and rebuild asks
// for a restart when they change; without one they are written to disk.
func devGenerators(root string, rebuild func()) []devGenerator {
	var out []devGenerator
	if viewgen.HasTemplates(root) {
		if rebuild != nil {
			out = append(out, viewsCheckGenerator(root, rebuild))
		} else {
			out = append(out, viewsGenerator(root))
		}
	}
	return out
}

// viewFilesIgnoreHint is a note for a git project whose .gitignore lets
// the view files nexus dev writes into the tree be committed, else "".
func viewFilesIgnoreHint(root string) string {
	if !viewgen.HasTemplates(root) {
		return ""
	}
	var missing []string
	for _, name := range []string{"x_templ.go", "view_gen.go", "view_imports_gen.go"} {
		err := exec.Command("git", "-C", root, "check-ignore", "-q", name).Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			continue // ignored, or not a git work tree
		}
		if name == "x_templ.go" {
			name = "*_templ.go"
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return ""
	}
	return "compiled views are written beside the .templ files: add " + strings.Join(missing, ", ") +
		" to .gitignore (or run nexus dev --no-view-files)"
}

// viewsWatch is what the views generators react to: a .templ save, or a Go
// edit that may declare what templates use (a state struct, a view.Shard
// registration).
func viewsWatch(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".templ") {
		return true
	}
	generated := strings.HasSuffix(base, "_templ.go") || base == "view_gen.go" || base == "view_imports_gen.go"
	return strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go") && !generated
}

// viewsError shows a views compile error with positions relative to the
// project (they are absolute).
func viewsError(root string, err error) error {
	if abs, aerr := filepath.Abs(root); aerr == nil {
		return errors.New(strings.ReplaceAll(err.Error(), abs+string(filepath.Separator), ""))
	}
	return err
}

// viewsGenerator compiles the project's reactive templ views (package
// github.com/paulmanoni/nexus/v2/view) and writes the generated Go to disk,
// so any editor and a plain go build see it (nexus dev, by default).
func viewsGenerator(root string) devGenerator {
	notes := &warningNotes{}
	return devGenerator{
		name:    "views",
		watches: viewsWatch,
		notes:   notes.take,
		run: func() (string, error) {
			changed, warnings, err := viewgen.WriteModule(root)
			if err != nil {
				return "", viewsError(root, err)
			}
			notes.set(root, warnings)
			if len(changed) == 0 {
				return "", nil
			}
			return fmt.Sprintf("%d file(s) updated", len(changed)), nil
		},
	}
}

// viewsCheckGenerator compiles the views without writing them: the dev
// build takes them from its overlay, and the editor from nexus lsp. It
// reports compile errors as they happen and calls rebuild when the
// compiled output changes — a .templ save is not a Go build input, so the
// watcher alone would not restart the app.
func viewsCheckGenerator(root string, rebuild func()) devGenerator {
	var last string
	notes := &warningNotes{}
	return devGenerator{
		name:    "views",
		watches: viewsWatch,
		notes:   notes.take,
		run: func() (string, error) {
			plan, err := viewgen.Generate(root)
			if err != nil {
				return "", viewsError(root, err)
			}
			notes.set(root, plan.Warnings)
			h := sha256.New()
			paths := make([]string, 0, len(plan.Files))
			for path := range plan.Files {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				fmt.Fprintf(h, "%s\x00%d\x00", path, len(plan.Files[path]))
				h.Write(plan.Files[path])
			}
			sum := hex.EncodeToString(h.Sum(nil))
			first := last == ""
			if sum == last {
				return "", nil
			}
			last = sum
			if first {
				return "compiled in memory", nil
			}
			rebuild()
			return "recompiled", nil
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
	if g.notes == nil {
		return
	}
	for _, line := range g.notes() {
		fmt.Fprintf(r.out, "    %s[%s]%s %s\n", ansiYellow, g.name, ansiReset, line)
	}
}
