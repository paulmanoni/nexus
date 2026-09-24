package vitehot

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func write(t *testing.T, dist, body string) {
	t.Helper()
	p := Path(dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func on() bool { return true }

// me is a pid that is certainly alive: this test process.
var me = os.Getpid()

// closedOrigin is an origin nothing listens on. Tests never probe a real
// port such as 5173, which may be a developer's own dev server.
func closedOrigin(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	if !alive(me) {
		t.Skip("no liveness check on this platform")
	}
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Skipf("no `true` binary: %v", err)
	}
	return c.ProcessState.Pid()
}

// fakeVite answers GET <base>@vite/client, and counts the requests.
func fakeVite(t *testing.T, base string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != base+"@vite/client" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, "export {}")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func hotJSON(origin string, pid int) string {
	return fmt.Sprintf(`{"version":1,"origin":%q,"base":"/","entries":["src/main.ts"],"pid":%d}`, origin, pid)
}

func TestAbsentFileMeansNoDevServer(t *testing.T) {
	h, err := NewReader(t.TempDir(), on).Current()
	if h != nil || err != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", h, err)
	}
}

func TestDisabledIgnoresAPresentFile(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, hotJSON(closedOrigin(t), me))
	h, err := NewReader(dist, func() bool { return false }).Current()
	if h != nil || err != nil {
		t.Fatalf("a disabled reader must ignore the file, got (%v, %v)", h, err)
	}
}

func TestReadsAndResolvesURLs(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, fmt.Sprintf(`{"version":1,"origin":"http://127.0.0.1:5174/","base":"/app","entries":["src/main.ts"],"pid":%d}`, me))
	h, err := NewReader(dist, on).Current()
	if err != nil || h == nil {
		t.Fatalf("got (%v, %v)", h, err)
	}
	if h.Origin != "http://127.0.0.1:5174" {
		t.Fatalf("Origin = %q, want it normalised without the slash", h.Origin)
	}
	if got := h.ClientURL(); got != "http://127.0.0.1:5174/app/@vite/client" {
		t.Fatalf("ClientURL = %q", got)
	}
	if got := h.URL(h.Entry()); got != "http://127.0.0.1:5174/app/src/main.ts" {
		t.Fatalf("entry URL = %q", got)
	}
}

func TestPicksUpARestartOnADifferentPort(t *testing.T) {
	dist := t.TempDir()
	r := NewReader(dist, on)
	write(t, dist, hotJSON("http://127.0.0.1:5173", me))
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5173" {
		t.Fatalf("first read: %+v", h)
	}
	// Same size on purpose, so only the mtime tells the reader it changed.
	time.Sleep(20 * time.Millisecond)
	write(t, dist, hotJSON("http://127.0.0.1:5199", me))
	now := time.Now().Add(time.Second)
	_ = os.Chtimes(Path(dist), now, now)
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5199" {
		t.Fatalf("restart not picked up: %+v", h)
	}
}

func TestRemovalIsNoticed(t *testing.T) {
	dist := t.TempDir()
	r := NewReader(dist, on)
	write(t, dist, hotJSON("http://127.0.0.1:5173", me))
	if h, _ := r.Current(); h == nil {
		t.Fatal("expected a hot file")
	}
	_ = os.Remove(Path(dist))
	if h, err := r.Current(); h != nil || err != nil {
		t.Fatalf("after removal want (nil, nil), got (%v, %v)", h, err)
	}
}

func TestUnusableFilesAreErrorsNotSilence(t *testing.T) {
	cases := map[string]string{
		"malformed":      `{"version":1,"origin":`,
		"wrong version":  `{"version":99,"origin":"http://x"}`,
		"missing origin": `{"version":1}`,
		"markup origin":  `{"version":1,"origin":"http://127.0.0.1:5173\"><script>alert(1)</script><x a=\""}`,
		"path origin":    `{"version":1,"origin":"http://127.0.0.1:5173/app"}`,
	}
	for name, body := range cases {
		dist := t.TempDir()
		write(t, dist, body)
		r := NewReader(dist, on)
		r.probe = func(context.Context, string) bool { t.Errorf("%s: probed an unusable file", name); return false }
		h, err := r.Current()
		if h != nil || err == nil {
			t.Fatalf("%s: want an error, got (%v, %v)", name, h, err)
		}
		if !strings.Contains(err.Error(), r.Path()) {
			t.Errorf("%s: error should name the file: %v", name, err)
		}
	}
}

// A file left by a dead dev server is absent, not an error: nexus dev stops
// Vite with SIGKILL, so this is routine. It is logged once per file and pid.
func TestDeadDevServerFileReadsAsAbsent(t *testing.T) {
	dist := t.TempDir()
	pid := deadPID(t)
	write(t, dist, hotJSON(closedOrigin(t), pid))
	r := NewReader(dist, on)
	var logs []string
	r.logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	for i := 0; i < 3; i++ {
		h, err := r.Current()
		if h != nil || err != nil {
			t.Fatalf("want (nil, nil), got (%v, %v)", h, err)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d times, want once: %q", len(logs), logs)
	}
	if !strings.Contains(logs[0], r.Path()) || !strings.Contains(logs[0], fmt.Sprint(pid)) {
		t.Errorf("log should name the file and pid: %s", logs[0])
	}

	// A different dead pid is a different leftover, reported again.
	write(t, dist, hotJSON(closedOrigin(t), deadPID(t)))
	r.Current()
	if len(logs) != 2 {
		t.Errorf("a new stale file was not reported: %q", logs)
	}
}

// A dev server in a container sharing the volume writes a pid from its own
// namespace: dead (or someone else's) here, yet the server is up. The origin
// answering is what counts.
func TestDeadPIDButAnsweringOriginIsLive(t *testing.T) {
	for _, pid := range []int{0, deadPID(t)} {
		srv, hits := fakeVite(t, "/")
		dist := t.TempDir()
		write(t, dist, hotJSON(srv.URL, pid))
		r := NewReader(dist, on)
		h, err := r.Current()
		if err != nil || h == nil || h.Origin != srv.URL {
			t.Fatalf("pid %d: want the live file, got (%v, %v)", pid, h, err)
		}
		// Cached: a burst of requests, one probe.
		for i := 0; i < 20; i++ {
			r.Current()
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("pid %d: probed %d times in a burst, want 1", pid, n)
		}
	}
}

// Another server on a reused port is not a dev server.
func TestProbeRequiresSuccess(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	redirect := httptest.NewServer(http.RedirectHandler("http://example.invalid/", http.StatusFound))
	t.Cleanup(redirect.Close)
	for _, origin := range []string{notFound.URL, redirect.URL} {
		dist := t.TempDir()
		write(t, dist, hotJSON(origin, 0))
		r := NewReader(dist, on)
		r.logf = nil
		if h, err := r.Current(); h != nil || err != nil {
			t.Errorf("%s: want (nil, nil), got (%v, %v)", origin, h, err)
		}
	}
}

// A dev server busy compiling accepts the connection but answers after the
// probe deadline: that is alive, not a stop. A refused origin is dead.
func TestBusyDevServerIsAlive(t *testing.T) {
	release := make(chan struct{})
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); busy.Close() })
	for _, c := range []struct {
		origin string
		alive  bool
	}{{busy.URL, true}, {closedOrigin(t), false}} {
		dist := t.TempDir()
		write(t, dist, hotJSON(c.origin, 0))
		r := NewReader(dist, on)
		r.logf = nil
		start := time.Now()
		h, err := r.Current()
		if err != nil || (h != nil) != c.alive {
			t.Errorf("%s: got (%v, %v), want alive=%v", c.origin, h, err, c.alive)
		}
		if d := time.Since(start); d > ProbeTimeout+time.Second {
			t.Errorf("%s: probe took %v", c.origin, d)
		}
	}
}

func TestLivePIDIsNotProbed(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, hotJSON(closedOrigin(t), me))
	r := NewReader(dist, on)
	r.probe = func(context.Context, string) bool { t.Error("probed although the pid is alive"); return false }
	if h, err := r.Current(); h == nil || err != nil {
		t.Fatalf("got (%v, %v)", h, err)
	}
}

func TestProbeIsBoundedAndCached(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, hotJSON(closedOrigin(t), 0))
	r := NewReader(dist, on)
	r.logf = nil
	var n atomic.Int32
	r.probe = func(ctx context.Context, _ string) bool {
		n.Add(1)
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > ProbeTimeout {
			t.Errorf("probe context not bounded by ProbeTimeout")
		}
		return false
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Current() }()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("concurrent callers probed %d times, want 1", n.Load())
	}
	// The result expires, so a dev server that comes back is noticed.
	r.mu.Lock()
	for k, v := range r.probed {
		v.at = v.at.Add(-2 * probeTTL)
		r.probed[k] = v
	}
	r.mu.Unlock()
	r.Current()
	if n.Load() != 2 {
		t.Fatalf("expired probe not repeated: %d", n.Load())
	}
}

func TestEnabledRule(t *testing.T) {
	cases := []struct {
		dev  bool
		env  string
		want bool
	}{
		{true, "", true},
		{false, "development", true},
		{false, "Development", true},
		{false, "production", false},
		{false, "", false},
		{false, "staging", false},
	}
	for _, c := range cases {
		if got := Enabled(c.dev, c.env); got != c.want {
			t.Errorf("Enabled(%v, %q) = %v, want %v", c.dev, c.env, got, c.want)
		}
	}
}

func TestSameLengthRewriteIsSeenImmediately(t *testing.T) {
	dist := t.TempDir()
	r := NewReader(dist, on)
	write(t, dist, hotJSON("http://127.0.0.1:5173", me))
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5173" {
		t.Fatalf("first read: %+v", h)
	}
	// Same byte length, written back to back: a cache keyed on mtime and
	// size could return the old origin here on a coarse-timestamp filesystem.
	write(t, dist, hotJSON("http://127.0.0.1:5174", me))
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5174" {
		t.Fatalf("same-length rewrite missed: %+v", h)
	}
}

func TestModuleEntrySkipsHTML(t *testing.T) {
	cases := []struct {
		entries  []string
		want     string
		wantHTML bool
	}{
		{[]string{"index.html"}, "", true},
		{[]string{"index.html", "src/main.ts"}, "src/main.ts", true},
		{[]string{"INDEX.HTML", "src/app.tsx"}, "src/app.tsx", true},
		{[]string{"src/main.ts"}, "src/main.ts", false},
		{nil, "", true}, // Vite's default input is index.html
	}
	for _, c := range cases {
		h := &Hot{Entries: c.entries}
		if got := h.ModuleEntry(); got != c.want {
			t.Errorf("ModuleEntry(%v) = %q, want %q", c.entries, got, c.want)
		}
		if got := h.HasHTMLEntry(); got != c.wantHTML {
			t.Errorf("HasHTMLEntry(%v) = %v, want %v", c.entries, got, c.wantHTML)
		}
	}
}

func TestPathIsAbsolute(t *testing.T) {
	if p := NewReader("web/dist", on).Path(); !filepath.IsAbs(p) {
		t.Fatalf("Path() = %q, want absolute so messages name one file", p)
	}
}

func TestAbsoluteBaseUsesOnlyItsPathInDev(t *testing.T) {
	h := &Hot{Origin: "http://localhost:5174", Base: "https://cdn.example.com/app/"}
	if got := h.URL("src/main.ts"); got != "http://localhost:5174/app/src/main.ts" {
		t.Fatalf("URL = %q", got)
	}
	for base, want := range map[string]string{"./": "/", "": "/", "/": "/", "app": "/app/"} {
		h := &Hot{Origin: "http://localhost:5174", Base: base}
		if got := h.URL("x.js"); got != "http://localhost:5174"+want+"x.js" {
			t.Errorf("base %q: URL = %q", base, got)
		}
	}
}

// The base is written into pages next to the origin; whatever it holds, the
// prefix must not be able to end a quoted attribute or JS string.
func TestBaseIsEscaped(t *testing.T) {
	for _, base := range []string{`/a"><script>x</script>/`, `/it's/`, `/a b/`, `/a\b/`, `/<x>/`} {
		h := &Hot{Origin: "http://localhost:5174", Base: base}
		got := h.URL("src/main.ts")
		if strings.ContainsAny(strings.TrimPrefix(got, "http://localhost:5174"), `"'<> \`) {
			t.Errorf("base %q: URL %q carries a quote, bracket, space or backslash", base, got)
		}
	}
	if got := (&Hot{Origin: "http://h", Base: "/my%20app/"}).URL("x"); got != "http://h/my%20app/x" {
		t.Errorf("an already-escaped base must not be escaped twice: %q", got)
	}
}

func TestValidateOrigin(t *testing.T) {
	good := map[string]string{
		"http://127.0.0.1:5173":   "http://127.0.0.1:5173",
		"http://127.0.0.1:5173/":  "http://127.0.0.1:5173",
		"https://localhost":       "https://localhost",
		"http://[::1]:5173":       "http://[::1]:5173",
		"http://my-box.local:517": "http://my-box.local:517",
	}
	for in, want := range good {
		if got, err := ValidateOrigin(in); err != nil || got != want {
			t.Errorf("ValidateOrigin(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		`http://127.0.0.1:5173"><script>alert(1)</script><x a="`,
		`http://a"b:5173`,
		`http://a<b>`,
		"javascript:alert(1)",
		"file:///etc/passwd",
		"ftp://127.0.0.1",
		"//127.0.0.1:5173",
		"127.0.0.1:5173",
		"http://user:pw@127.0.0.1:5173",
		"http://127.0.0.1:5173/app",
		"http://127.0.0.1:5173/?x=1",
		"http://127.0.0.1:5173?",
		"http://127.0.0.1:5173#frag",
		"http://127.0.0.1:",
		"http://[fe80::1%25en0]:5173",
		"http://",
		"",
	} {
		if got, err := ValidateOrigin(in); err == nil {
			t.Errorf("ValidateOrigin(%q) = %q, want an error", in, got)
		}
	}
}
