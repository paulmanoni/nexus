package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paulmanoni/nexus/internal/vitehot"
)

// The frontend half of `nexus dev`: the project's own Vite, supervised.
//
// nexus dev starts `vite` in the frontend directory when it has a
// package.json, and never asks it where it is: nexus-vite-plugin writes the
// dev server's bound origin into <outDir>/.vite/nexus-hot.json, the app reads
// that file per request (App.ViteHot), and so does this supervisor — the
// file appearing is the readiness signal. The browser always opens the app;
// Vite serves modules and HMR only, which is why its own "Local:" banner is
// filtered out of the log.

// devFrontendDir resolves the frontend project directory for the package in
// pkgDir (absolute). Precedence:
//
//  1. --frontend (flag, as typed: relative to the working directory);
//  2. NEXUS_FRONTEND_DIR (relative to the project, as for nexus build);
//  3. the ServeFrontend / frontend.Plugin call in the package's source;
//  4. <pkgDir>/web when it holds a package.json — nexus build's default,
//     for an app whose ServeFrontend root is not a string literal.
//
// Returns "" when there is no frontend, else an absolute path and where it
// came from (for --verbose).
func devFrontendDir(pkgDir, flag string) (dir, source string) {
	abs := func(base, p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	if flag != "" {
		wd, _ := os.Getwd()
		return abs(wd, flag), "--frontend"
	}
	if v := os.Getenv("NEXUS_FRONTEND_DIR"); v != "" {
		return abs(pkgDir, v), "NEXUS_FRONTEND_DIR"
	}
	if d := detectFrontendDir(pkgDir); d != "" {
		return abs(pkgDir, d), "detected in source"
	}
	if web := filepath.Join(pkgDir, "web"); fileExists(filepath.Join(web, "package.json")) {
		return web, "web/package.json"
	}
	return "", ""
}

// devFrontend is the frontend side of one nexus dev session.
type devFrontend struct {
	Dir     string // absolute; "" when the app has no frontend
	Project frontendProject
	Vite    *devVite // non-nil only for a Vite project (package.json); stop it
}

// startDevFrontend resolves the frontend for the package in pkgDir (see
// devFrontendDir) and starts its Vite when it is a Vite project. A
// viteless-era directory gets the migration hint, once, and no dev server —
// nexus dev carries on with the Go app. cfg supplies everything but the
// directories: WebDir comes from the resolution, ServedDist is the
// ServeFrontend root under pkgDir ("" when unknown).
func startDevFrontend(ctx context.Context, pkgDir, flag, servedDist string, cfg devViteConfig) devFrontend {
	dir, source := devFrontendDir(pkgDir, flag)
	if dir == "" {
		return devFrontend{}
	}
	f := devFrontend{Dir: dir, Project: inspectFrontend(dir)}
	if cfg.Verbose {
		fmt.Fprintf(cfg.Out, "%s●%s frontend %s (%s)\n", ansiCyan, ansiReset, dir, source)
	}
	switch {
	case f.Project.Legacy != "":
		fmt.Fprintf(cfg.Notes, "%s●%s %s Running without a frontend dev server.\n", ansiYellow, ansiReset, f.Project.legacyHint())
	case f.Project.PackageJSON:
		cfg.WebDir = f.Project.Dir
		cfg.ServedDist = servedDist
		f.Vite = startDevVite(ctx, cfg)
	}
	return f
}

// devViteConfig is what the supervisor needs to run one Vite dev server.
type devViteConfig struct {
	WebDir   string // absolute frontend project dir (has package.json)
	TOMLPath string // nexus.toml, for the [env] bridge
	// ServedDist is the directory the app reads the hot file from — the
	// ServeFrontend root under the package dir — or "" when unknown.
	ServedDist string
	Verbose    bool
	// Out receives Vite's filtered, [web]-prefixed log lines. Notes is
	// where the supervisor's own messages go (warnings, exit reports).
	Out, Notes io.Writer
	// Color forces colored output from Vite (it sees a pipe, not a tty).
	Color bool
	// HotTimeout bounds the wait for the hot file after Vite started; past
	// it the supervisor warns and stops waiting. 0 = devHotTimeout.
	HotTimeout time.Duration
	// Grace is how long stop waits between SIGTERM and SIGKILL.
	// 0 = viteKillGrace.
	Grace time.Duration
}

// devHotTimeout is how long a started Vite gets to write its hot file
// before the supervisor says something is off. The plugin writes it the
// moment the socket listens — well before dependency optimisation — so a
// healthy start takes well under a second; this covers a slow cold start.
const devHotTimeout = 20 * time.Second

// viteKillGrace is how long stop waits for Vite after SIGTERM. Vite's CLI
// answers SIGTERM by closing the server (watchers, HMR socket, optimiser)
// and exiting — measured at well under 100ms for a small app — and the
// plugin removes the hot file synchronously on the signal itself, before
// that close runs, so the grace never decides whether the hot file is
// left behind. It is longer than the app's devKillGrace only because a
// large project's watcher teardown can take a few hundred milliseconds,
// and cutting it short leaves nothing useful to gain.
const viteKillGrace = 2 * time.Second

// devVite supervises one Vite dev server for the life of a nexus dev
// session. Construct with startDevVite; always call stop.
type devVite struct {
	cfg    devViteConfig
	ctx    context.Context
	cancel context.CancelFunc

	started chan struct{} // closed when the start attempt is over (spawned or not)
	settled chan struct{} // closed once the hot file appeared, Vite gave up, or the wait timed out
	exited  chan struct{} // closed after Vite exits; nil until spawned

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping bool
	hot      *vitehot.Hot
	hotDir   string
	waitErr  error

	settleOnce sync.Once
}

// startDevVite installs dependencies when needed, writes the SDK plugin,
// and spawns Vite — all in the background, so the Go build is not held up
// by `npm install`. It returns at once.
func startDevVite(ctx context.Context, cfg devViteConfig) *devVite {
	if cfg.HotTimeout == 0 {
		cfg.HotTimeout = devHotTimeout
	}
	if cfg.Grace == 0 {
		cfg.Grace = viteKillGrace
	}
	c, cancel := context.WithCancel(ctx)
	v := &devVite{
		cfg:     cfg,
		ctx:     c,
		cancel:  cancel,
		started: make(chan struct{}),
		settled: make(chan struct{}),
	}
	go v.start()
	return v
}

// settledCh is closed once the hot file is there (or will not be): the
// ready line waits on it, so a browser opened on "ready" gets live modules.
func (v *devVite) settledCh() <-chan struct{} { return v.settled }

func (v *devVite) settle() { v.settleOnce.Do(func() { close(v.settled) }) }

func (v *devVite) notef(format string, args ...any) {
	fmt.Fprintf(v.cfg.Notes, "%s●%s %s\n", ansiYellow, ansiReset, fmt.Sprintf(format, args...))
}

func (v *devVite) start() {
	defer close(v.started)
	webDir := v.cfg.WebDir
	if err := ensureNodeModules(v.ctx, webDir, v.cfg.Out, v.cfg.Out); err != nil {
		if v.ctx.Err() == nil {
			v.notef("frontend dev server not started: %v", err)
		}
		v.settle()
		return
	}
	if err := writeSDKPlugin(webDir, v.cfg.Notes); err != nil {
		// A config that imports it fails to load and says so below; one
		// that does not import it runs fine.
		v.notef("could not write %s: %v", filepath.Join(webDir, "sdk"), err)
	}
	env, err := frontendEnv(v.cfg.TOMLPath, v.cfg.Notes)
	if err != nil {
		v.notef("[env] not passed to Vite: %v", err)
		env = os.Environ()
	}
	if v.cfg.Color && os.Getenv("NO_COLOR") == "" && os.Getenv("FORCE_COLOR") == "" {
		env = append(env, "FORCE_COLOR=1")
	}
	// Not v.ctx: exec.CommandContext would SIGKILL Vite the instant the
	// session is cancelled, before stop's SIGTERM gives the plugin its
	// chance to remove the hot file.
	cmd, err := viteCmd(context.Background(), webDir, env)
	if err != nil {
		v.notef("frontend dev server not started: %v", err)
		v.settle()
		return
	}
	lw := newViteLogWriter(v.cfg.Out, v.cfg.Verbose)
	cmd.Stdout = lw
	cmd.Stderr = lw

	v.mu.Lock()
	if v.stopping {
		v.mu.Unlock()
		v.settle()
		return
	}
	since := time.Now()
	if err := cmd.Start(); err != nil {
		v.mu.Unlock()
		v.notef("frontend dev server not started: %v", err)
		v.settle()
		return
	}
	v.cmd = cmd
	v.exited = make(chan struct{})
	v.mu.Unlock()

	go func() {
		err := cmd.Wait()
		lw.flush()
		v.mu.Lock()
		v.waitErr = err
		stopping := v.stopping
		v.mu.Unlock()
		if !stopping {
			if err != nil {
				v.notef("vite exited: %v · the app serves its built bundle until nexus dev restarts", err)
			} else {
				v.notef("vite exited · the app serves its built bundle until nexus dev restarts")
			}
		}
		close(v.exited)
		v.settle()
	}()

	v.awaitHot(cmd.Process.Pid, since)
}

// hotDirs are where this Vite's hot file may appear: the directory the app
// reads first, then Vite's default outDir.
func (v *devVite) hotDirs() []string {
	var dirs []string
	if v.cfg.ServedDist != "" {
		dirs = append(dirs, v.cfg.ServedDist)
	}
	if d := filepath.Join(v.cfg.WebDir, "dist"); d != v.cfg.ServedDist {
		dirs = append(dirs, d)
	}
	return dirs
}

// awaitHot polls for the hot file this Vite writes, until it appears, Vite
// exits, the session ends, or HotTimeout passes.
func (v *devVite) awaitHot(pid int, since time.Time) {
	defer v.settle()
	dirs := v.hotDirs()
	deadline := time.NewTimer(v.cfg.HotTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if h, dir := findDevHot(dirs, pid, since); h != nil {
			v.mu.Lock()
			v.hot, v.hotDir = h, dir
			v.mu.Unlock()
			if v.cfg.ServedDist != "" && dir != v.cfg.ServedDist {
				v.notef("Vite wrote its hot file to %s, but the app reads %s — set build.outDir in vite.config to the directory ServeFrontend serves, or the app will not use the dev server",
					vitehot.Path(dir), vitehot.Path(v.cfg.ServedDist))
			}
			return
		}
		select {
		case <-v.ctx.Done():
			return
		case <-v.exited:
			return
		case <-deadline.C:
			paths := make([]string, len(dirs))
			for i, d := range dirs {
				paths[i] = vitehot.Path(d)
			}
			v.notef("vite is running but wrote no hot file (%s) within %s — add nexus() from './sdk/nexus-vite-plugin.js' to the plugins in vite.config; until then the app serves its built bundle",
				strings.Join(paths, " or "), v.cfg.HotTimeout)
			return
		case <-tick.C:
		}
	}
}

// findDevHot returns the hot file written by the Vite started at since with
// pid, from the first of dirs that has one. The pid match is exact where the
// executable is Vite itself; the modification time covers a launcher that
// runs Vite as a child (vite.cmd on Windows). A file from another, still
// running dev server (an `npm run dev` in another terminal) matches neither
// and is ignored until this Vite replaces it.
//
// Ownership is decided before the file is handed to vitehot's reader, which
// logs a file naming a dead dev server: a stale file from an earlier session
// is routine while nexus dev starts Vite, and is about to be replaced.
func findDevHot(dirs []string, pid int, since time.Time) (*vitehot.Hot, string) {
	for _, dir := range dirs {
		path := vitehot.Path(dir)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var raw struct {
			PID int `json:"pid"`
		}
		_ = json.Unmarshal(b, &raw)
		if raw.PID != pid && fi.ModTime().Before(since.Add(-time.Second)) {
			continue
		}
		// Ours: the reader validates it (schema, origin) and checks the
		// server is alive.
		if h, err := vitehot.NewReader(dir, func() bool { return true }).Current(); err == nil && h != nil {
			return h, dir
		}
	}
	return nil, ""
}

// origin reports the dev server's origin once its hot file appeared.
func (v *devVite) origin() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.hot == nil {
		return ""
	}
	return v.hot.Origin
}

// stop ends Vite: SIGTERM to its process group, SIGKILL after the grace,
// and it returns only once Vite has exited — so the plugin's hot-file
// cleanup has run (or, after SIGKILL, the file names a dead pid, which
// readers treat as absent). Safe to call more than once.
func (v *devVite) stop() {
	v.mu.Lock()
	v.stopping = true
	v.mu.Unlock()
	v.cancel() // aborts an npm install in progress
	<-v.started
	v.mu.Lock()
	cmd, exited := v.cmd, v.exited
	v.mu.Unlock()
	if cmd == nil {
		return
	}
	stopProcessGroup(cmd.Process.Pid, exited, v.cfg.Grace, func() {
		v.notef("vite didn't exit within %s · SIGKILL", v.cfg.Grace)
	})
}

// stopProcessGroup sends SIGTERM to pid's group, waits up to grace for
// exited to close, then sends SIGKILL and waits again. onKill runs before
// the SIGKILL, for a message.
func stopProcessGroup(pid int, exited <-chan struct{}, grace time.Duration, onKill func()) {
	select {
	case <-exited:
		return
	default:
	}
	_ = killProcessGroup(pid, syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(grace):
	}
	if onKill != nil {
		onKill()
	}
	_ = killProcessGroup(pid, syscall.SIGKILL)
	<-exited
}

// viteLogWriter turns Vite's output into [web]-prefixed lines, dropping the
// startup banner's address lines: they name Vite's own port, and the page
// is served by the app — a reader who follows "Local: http://…:5173" lands
// on a page whose API calls have nowhere to go. --verbose keeps everything.
type viteLogWriter struct {
	w       io.Writer
	verbose bool
	mu      sync.Mutex
	buf     []byte
}

func newViteLogWriter(w io.Writer, verbose bool) *viteLogWriter {
	return &viteLogWriter{w: w, verbose: verbose}
}

func (l *viteLogWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

// flush writes a trailing partial line (Vite exiting mid-line).
func (l *viteLogWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(string(l.buf))
		l.buf = nil
	}
}

func (l *viteLogWriter) emit(line string) {
	line = strings.TrimRight(line, "\r")
	if !l.verbose && viteNoiseLine(line) {
		return
	}
	fmt.Fprintf(l.w, "%s[web]%s %s\n", ansiCyan, ansiReset, line)
}

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// viteNoiseLine reports whether a Vite output line is banner furniture the
// dev loop hides: the Local/Network address lines, the keyboard-shortcut
// hint (there is no stdin to press h on), and blank spacer lines.
func viteNoiseLine(line string) bool {
	s := strings.TrimSpace(ansiEscapeRE.ReplaceAllString(line, ""))
	s = strings.TrimSpace(strings.TrimLeft(s, "➜>-* "))
	if s == "" {
		return true
	}
	switch {
	case strings.HasPrefix(s, "Local:"), strings.HasPrefix(s, "Network:"):
		return true
	case strings.Contains(s, "press h + enter"), strings.Contains(s, "press h to show help"):
		return true
	}
	return false
}
