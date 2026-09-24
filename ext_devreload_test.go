package nexus

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus/httpx/stdrouter"
)

type sseEvent struct{ name, data, retry string }

// openDevReload connects to the SSE route and streams its events.
func openDevReload(t *testing.T, base string) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/__nexus/dev/reload", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan sseEvent, 16)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" || ev.data != "" {
					out <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "retry: "):
				ev.retry = strings.TrimPrefix(line, "retry: ")
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, ch <-chan sseEvent, within time.Duration) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev, true
	case <-time.After(within):
		return sseEvent{}, false
	}
}

func bootID(t *testing.T, ev sseEvent) string {
	t.Helper()
	if ev.name != "boot" {
		t.Fatalf("first event = %+v, want boot", ev)
	}
	var v struct{ ID string }
	if err := json.Unmarshal([]byte(ev.data), &v); err != nil || v.ID == "" {
		t.Fatalf("boot data %q: %v", ev.data, err)
	}
	return v.ID
}

func TestDevReloadStreamOpensWithBootID(t *testing.T) {
	r := stdrouter.New()
	mountDevReload(r, "", nil, nil)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close) // registered first, so it runs after the streams are cancelled

	ev, ok := nextEvent(t, openDevReload(t, srv.URL), 2*time.Second)
	if !ok {
		t.Fatal("no boot event")
	}
	id := bootID(t, ev)
	if id != devBootID() {
		t.Fatalf("boot id %q, want this process's %q", id, devBootID())
	}
	if ev.retry != "250" {
		t.Fatalf("retry = %q, want a short reconnect delay", ev.retry)
	}
	ev2, _ := nextEvent(t, openDevReload(t, srv.URL), 2*time.Second)
	if bootID(t, ev2) != id {
		t.Fatal("a second stream to the same process named a different boot")
	}
	if len(id) != 16 {
		t.Fatalf("boot id %q: want 8 random bytes in hex", id)
	}
}

func TestDevReloadScriptCarriesBootID(t *testing.T) {
	r := stdrouter.New()
	mountDevReload(r, "", nil, nil)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close) // registered first, so it runs after the streams are cancelled
	resp, err := http.Get(srv.URL + "/__nexus/dev/script.js")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(string(b), `var boot = "`+devBootID()+`";`) {
		t.Fatalf("script does not carry the boot id:\n%s", b)
	}
	if strings.Contains(string(b), devReloadBootMarker) {
		t.Fatal("marker left in the served script")
	}
}

// TestDevReloadFileEvents drives the real watcher: file changes reload
// the page unless a dev server owns the frontend, Go sources and Vite
// metadata never do, and a dev server starting or stopping reloads once.
func TestDevReloadFileEvents(t *testing.T) {
	defer func(d time.Duration) { devReloadPollInterval = d }(devReloadPollInterval)
	devReloadPollInterval = 50 * time.Millisecond

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist", ".vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	var live atomic.Bool
	r := stdrouter.New()
	mountDevReload(r, dir, nil, func() string {
		if live.Load() {
			return "http://127.0.0.1:5999"
		}
		return ""
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close) // registered first, so it runs after the streams are cancelled
	ch := openDevReload(t, srv.URL)
	if ev, ok := nextEvent(t, ch, 2*time.Second); !ok || ev.name != "boot" {
		t.Fatalf("want boot first, got %+v", ev)
	}

	n := 0
	touch := func(name string) {
		n++
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Longer than the 80ms debounce plus fsnotify latency.
	const quiet = 600 * time.Millisecond
	expectReload := func(what string) {
		t.Helper()
		ev, ok := nextEvent(t, ch, 3*time.Second)
		if !ok || ev.name != "reload" {
			t.Fatalf("%s: want a reload, got %+v (ok=%v)", what, ev, ok)
		}
		if ev, ok := nextEvent(t, ch, quiet); ok {
			t.Fatalf("%s: want exactly one reload, then got %+v", what, ev)
		}
	}
	expectNone := func(what string) {
		t.Helper()
		if ev, ok := nextEvent(t, ch, quiet); ok {
			t.Fatalf("%s: want no event, got %+v", what, ev)
		}
	}

	touch("app.css")
	expectReload("no dev server: a frontend file")

	touch("main.go")
	touch("go.mod")
	touch("dist/.vite/manifest.json")
	touch("dist/.vite/nexus-hot.json")
	expectNone("Go build inputs and Vite metadata")

	live.Store(true)
	expectReload("a dev server started")

	touch("app.css")
	touch("App.vue")
	expectNone("dev server live: frontend files are its to deliver")

	live.Store(false)
	expectReload("the dev server stopped")

	touch("app.css")
	expectReload("no dev server again: file watch as before")
}

func TestDevReloadGate(t *testing.T) {
	owner := ""
	g := newDevReloadGate(func() string { return owner })
	if !g.fileChanged() || g.ownerChanged() {
		t.Fatal("no dev server: file changes reload, nothing to report")
	}
	owner = "http://127.0.0.1:5173"
	if g.fileChanged() {
		t.Fatal("dev server live: file changes are its to deliver")
	}
	if g.ownerChanged() {
		t.Fatal("one poll is not enough to report a change")
	}
	if !g.ownerChanged() {
		t.Fatal("a change held for two polls is reported")
	}
	if g.ownerChanged() || g.ownerChanged() {
		t.Fatal("reported once")
	}
	// A restart on the same port: the hot file is gone for one poll.
	owner = ""
	if g.ownerChanged() {
		t.Fatal("a one-poll gap is not a change")
	}
	owner = "http://127.0.0.1:5173"
	if g.ownerChanged() || g.ownerChanged() {
		t.Fatal("a restart on the same origin must not reload")
	}
	// Vite comes back on a different port: open pages still point at the
	// old one, so they must reload to pick up the new origin.
	owner = "http://127.0.0.1:5174"
	g.ownerChanged()
	if !g.ownerChanged() {
		t.Fatal("a dev server on a new origin is reported")
	}
	owner = ""
	g.ownerChanged()
	if !g.ownerChanged() {
		t.Fatal("stopping is reported")
	}

	owner = "http://127.0.0.1:5173"
	if g := newDevReloadGate(func() string { return owner }); g.ownerChanged() || g.ownerChanged() {
		t.Fatal("a gate created while live starts settled on that origin")
	}
	if g := newDevReloadGate(nil); !g.fileChanged() || g.ownerChanged() || g.ownerChanged() {
		t.Fatal("no owner func means no dev server")
	}
}

// TestDevReloadShim runs the served script under node against a fake
// EventSource, one fresh context per scenario.
func TestDevReloadShim(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	script := strings.Replace(devReloadShim, devReloadBootMarker, `"A"`, 1)
	harness := `
const vm = require('node:vm')
const script = ` + strconvQuoteJS(script) + `
function run(steps) {
  const sources = [], timers = []
  let reloads = 0
  class ES {
    constructor(url) { this.url = url; this.readyState = 0; this.l = {}; sources.push(this) }
    addEventListener(t, f) { (this.l[t] = this.l[t] || []).push(f) }
    close() { this.readyState = 2 }
    emit(t, data) { (this.l[t] || []).forEach((f) => f({ data })) }
  }
  const ctx = { EventSource: ES, console: { info() {} }, JSON,
    location: { reload() { reloads++ } },
    setTimeout: (f, ms) => timers.push({ f, ms }) }
  ctx.window = ctx
  vm.createContext(ctx)
  const api = {
    load() { vm.runInContext(script, ctx) },
    es: () => sources[sources.length - 1],
    tick() { const ts = timers.splice(0); ts.forEach((x) => x.f()); return ts.map((x) => x.ms) },
  }
  const log = steps(api) || {}
  api.tick()
  return { reloads, sources: sources.length, url: sources[0] && sources[0].url, ...log }
}
const out = {
  sameBoot: run((a) => { a.load(); a.es().emit('boot', '{"id":"A"}') }),
  reconnectNewBoot: run((a) => {
    a.load(); const es = a.es()
    es.emit('boot', '{"id":"A"}')
    es.readyState = 0; es.emit('error')          // dropped; the browser reconnects
    es.emit('boot', '{"id":"B"}')
  }),
  reconnectSameBoot: run((a) => {
    a.load(); const es = a.es()
    es.emit('boot', '{"id":"A"}'); es.readyState = 0; es.emit('error'); es.emit('boot', '{"id":"A"}')
  }),
  firstStreamIsNewProcess: run((a) => { a.load(); a.es().emit('boot', '{"id":"B"}') }),
  fileReload: run((a) => { a.load(); a.es().emit('boot', '{"id":"A"}'); a.es().emit('reload', '{}'); a.es().emit('reload', '{}') }),
  givenUp: run((a) => {
    a.load(); const first = a.es()
    first.readyState = 2; first.emit('error')
    const delays = a.tick()
    a.es().readyState = 2; a.es().emit('error')
    const delays2 = a.tick()
    a.es().emit('boot', '{"id":"B"}')
    return { delays: delays.concat(delays2), firstClosed: first.readyState === 2 }
  }),
  loadedTwice: run((a) => { a.load(); a.load() }),
  badData: run((a) => { a.load(); a.es().emit('boot', 'nope'); a.es().emit('boot', '{}') }),
}
console.log(JSON.stringify(out))
`
	f := filepath.Join(t.TempDir(), "harness.cjs")
	if err := os.WriteFile(f, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	type result struct {
		Reloads     int
		Sources     int
		URL         string
		Delays      []int
		FirstClosed bool
	}
	var got map[string]result
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	want := map[string]struct{ reloads, sources int }{
		"sameBoot":                {0, 1},
		"reconnectNewBoot":        {1, 1},
		"reconnectSameBoot":       {0, 1},
		"firstStreamIsNewProcess": {1, 1},
		"fileReload":              {1, 1},
		"givenUp":                 {1, 3},
		"loadedTwice":             {0, 1},
		"badData":                 {0, 1},
	}
	for name, w := range want {
		g := got[name]
		if g.Reloads != w.reloads || g.Sources != w.sources {
			t.Errorf("%s: reloads=%d sources=%d, want reloads=%d sources=%d", name, g.Reloads, g.Sources, w.reloads, w.sources)
		}
		if g.URL != "/__nexus/dev/reload" {
			t.Errorf("%s: stream url %q", name, g.URL)
		}
	}
	if d := got["givenUp"].Delays; len(d) != 2 || d[0] != 250 || d[1] != 500 {
		t.Errorf("givenUp: reopen delays %v, want [250 500] (backoff)", d)
	}
	if !got["givenUp"].FirstClosed {
		t.Error("givenUp: the abandoned stream should be closed")
	}
}

// strconvQuoteJS renders s as a JS string literal (JSON is a subset).
func strconvQuoteJS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A stopped app leaves no dev-reload poller or watcher running.
func TestDevReloadStopsWithApp(t *testing.T) {
	count := func() int {
		buf := make([]byte, 1<<22)
		n := runtime.Stack(buf, true)
		return strings.Count(string(buf[:n]), "nexus.mountDevReload.func")
	}
	before := count()
	dir := t.TempDir()
	t.Setenv(NexusDevEnv, "1")
	t.Setenv(NexusDevRootEnv, dir)
	for i := 0; i < 3; i++ {
		fsys := fstest.MapFS{"web/dist/index.html": {Data: []byte("<html>x</html>")}}
		_, stop, err := InProcess(Config{}, ServeFrontend(fsys, "web/dist"))
		if err != nil {
			t.Fatal(err)
		}
		if err := stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for count() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := count() - before; n > 0 {
		t.Errorf("%d dev-reload goroutines outlived their stopped apps", n)
	}
}
