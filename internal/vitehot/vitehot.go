// Package vitehot reads the file nexus-vite-plugin writes while `vite dev`
// runs. It is the dev half of the frontend contract: the plugin states where
// the dev server actually is, and the Go side reads that instead of scraping
// Vite's startup output, guessing a port, or guessing the entry module.
//
// The file lives at <outDir>/.vite/nexus-hot.json — beside the manifest
// `vite build` writes — because the build output directory is the one path
// both sides already share: Vite's build.outDir and the root the app passes
// to nexus.ServeFrontend.
//
// Schema, version 1:
//
//	{
//	  "version": 1,
//	  "origin":  "http://127.0.0.1:5173",
//	  "base":    "/",
//	  "entries": ["src/main.ts"],
//	  "pid":     47487
//	}
//
// It is only ever read from disk, never from an embedded bundle, and only when
// Enabled says the app is in development. A file is followed only while the
// dev server it names is alive (see Reader.Current): one left behind by a
// killed `vite dev` reads as absent, so it can neither redirect a page nor
// turn one into an error.
package vitehot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Dir is the directory under the build output that holds the hot file.
	Dir = ".vite"
	// File is the hot file's name inside Dir.
	File = "nexus-hot.json"
	// Version is the schema version this reader understands.
	Version = 1
)

// Path returns where the hot file lives for a build output directory.
func Path(distDir string) string { return filepath.Join(distDir, Dir, File) }

// Enabled is the rule for when a hot file is looked at at all: under
// `nexus dev`, or when the app declares environment = "development". It keeps
// a binary that says nothing (the environment defaults to "production") from
// reading the file.
//
// It is not what protects a deployment from a stale file: `nexus new` writes
// environment = "development" into nexus.toml and deployments ship it, so
// plenty of deployed binaries pass this gate. The protection is liveness —
// Current follows a file only while the dev server it names is running, and
// a file left by a dead one reads as absent.
func Enabled(isDev bool, environment string) bool {
	return isDev || strings.EqualFold(environment, "development")
}

// Hot is the decoded hot file.
type Hot struct {
	Version int `json:"version"`
	// Origin is normalised to scheme://host[:port] by the reader.
	Origin  string   `json:"origin"`
	Base    string   `json:"base"`
	Entries []string `json:"entries"`
	PID     int      `json:"pid"`
}

// URL resolves a path served by the dev server, honouring Vite's base.
//
// The origin is validated when the file is read and the base is
// path-escaped here, so the prefix URL puts before p contains no quote,
// angle bracket, backslash or whitespace — it is inert inside an HTML
// attribute and inside a JS string. p is appended as given.
func (h *Hot) URL(p string) string {
	return h.prefix() + strings.TrimPrefix(p, "/")
}

// prefix is origin + base, ending in "/".
func (h *Hot) prefix() string {
	base := h.Base
	// A full-URL base (a CDN) only applies to built assets; Vite ignores its
	// origin in development and serves under its path. A relative base
	// ("./", "") is served at "/" in development.
	if u, err := url.Parse(base); err == nil {
		base = u.EscapedPath()
	} else {
		base = (&url.URL{Path: base}).EscapedPath()
	}
	// EscapedPath keeps RFC 3986 sub-delims as written; of those, only the
	// apostrophe could end a quoted attribute or JS string.
	base = strings.ReplaceAll(base, "'", "%27")
	if base == "" || base == "." || base == "./" {
		base = "/"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return strings.TrimRight(h.Origin, "/") + base
}

// ClientURL is the Vite client script that drives HMR.
func (h *Hot) ClientURL() string { return h.URL("@vite/client") }

// Entry is the first declared entry, or "" when none was declared. It may be
// index.html — Vite's default input for an SPA. Use ModuleEntry when the
// caller needs something a <script> tag can load.
func (h *Hot) Entry() string {
	if len(h.Entries) == 0 {
		return ""
	}
	return h.Entries[0]
}

// ModuleEntry is the first declared entry that is a module rather than an
// HTML page, or "" when there is none. An app that renders its own shell (an
// Inertia page) needs this; one that serves index.html does not.
func (h *Hot) ModuleEntry() string {
	for _, e := range h.Entries {
		if e != "" && !isHTML(e) {
			return e
		}
	}
	return ""
}

// HasHTMLEntry reports whether the build this dev server stands for has an
// HTML page among its inputs — whether `vite build` will emit an index.html.
// No declared entries means Vite's default input, index.html. An app that
// declares only modules (nexus({ input: 'src/main.ts' })) builds no page,
// whatever index.html the dev server happens to serve from its root.
func (h *Hot) HasHTMLEntry() bool {
	if len(h.Entries) == 0 {
		return true
	}
	for _, e := range h.Entries {
		if isHTML(e) {
			return true
		}
	}
	return false
}

func isHTML(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".html") }

// Reader returns the current hot file. It re-reads the file on every call:
// the file is a few hundred bytes and is consulted only when serving the
// frontend in development, and caching on mtime and size would miss a
// same-length rewrite (port 5173 → 5174) landing within one timestamp tick.
// Re-reading is what makes a Vite restart on a different port apply on the
// next request, with no Go restart — the env-var mechanism it replaces was
// read once per process. Only the liveness probe is cached, and briefly.
//
// Safe for concurrent use.
type Reader struct {
	path    string
	enabled func() bool

	// probe and logf are replaced by tests.
	probe func(ctx context.Context, clientURL string) bool
	logf  func(format string, args ...any)

	mu     sync.Mutex
	probed map[string]probeResult // client URL + pid → last probe
	logged map[string]bool        // pid + origin already reported as dead
}

type probeResult struct {
	at    time.Time
	alive bool
}

// ProbeTimeout bounds one liveness probe of a dev server whose pid cannot
// vouch for it. A local dev server answers in milliseconds; this caps what a
// dead origin can cost.
const ProbeTimeout = 300 * time.Millisecond

// probeTTL is how long a probe result stands. Current runs per request, so
// without it a stale file would cost a connection attempt on every one.
const probeTTL = time.Second

// NewReader watches the hot file for a build output directory on disk.
// enabled is consulted on every call; pass a closure over Enabled.
func NewReader(distDir string, enabled func() bool) *Reader {
	p := Path(distDir)
	// Absolute, so a message tells the developer exactly which file,
	// whatever directory they are reading it from.
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return &Reader{path: p, enabled: enabled, probe: probeOrigin, logf: log.Printf}
}

// Path is the file this reader watches — for messages that tell a developer
// where to look.
func (r *Reader) Path() string { return r.path }

// Current returns the hot file when a dev server is announced and alive.
//
//   - (nil, nil): no dev server to use. The reader is disabled, the file is
//     absent, or the file names a dev server that is not running — what a
//     `vite dev` killed without a chance to clean up leaves behind (nexus
//     dev stops Vite with SIGKILL). A dead file is logged once per file and
//     pid and otherwise ignored: the app serves its build exactly as if the
//     file were not there.
//   - (hot, nil): a live dev server; follow it.
//   - (nil, err): a file is present but cannot be understood — malformed
//     JSON, an unknown schema version, a missing or invalid origin. That is
//     a plugin/nexus mismatch or a hand-edited file, not a routine leftover,
//     so callers should report it rather than silently fall back.
//
// Liveness: a pid that is alive is trusted as is. A dead or absent pid is not
// proof of death — a dev server in a container sharing the volume writes a
// pid from another namespace — so the origin is probed (a GET of the Vite
// client, bounded by ProbeTimeout) and the file is followed if the server
// answers, or accepts the connection and is still working on the answer.
// Probe results are cached for about a second.
func (r *Reader) Current() (*Hot, error) {
	if r == nil || r.enabled == nil || !r.enabled() {
		return nil, nil
	}
	h, err := load(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if h.PID > 0 && alive(h.PID) {
		return h, nil
	}
	if r.probeAlive(h) {
		return h, nil
	}
	r.logDead(h)
	return nil, nil
}

func (r *Reader) probeAlive(h *Hot) bool {
	key := fmt.Sprintf("%s|%d", h.ClientURL(), h.PID)
	// Held across the probe on purpose: concurrent requests wait for one
	// probe instead of each starting their own.
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.probed[key]; ok && time.Since(p.at) < probeTTL {
		return p.alive
	}
	probe := r.probe
	if probe == nil {
		probe = probeOrigin
	}
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	ok := probe(ctx, h.ClientURL())
	cancel()
	// A session sees a handful of dev servers; if one somehow piles up
	// more, forgetting them all is cheaper than tracking ages.
	if r.probed == nil || len(r.probed) > 64 {
		r.probed = map[string]probeResult{}
	}
	r.probed[key] = probeResult{at: time.Now(), alive: ok}
	return ok
}

func (r *Reader) logDead(h *Hot) {
	key := fmt.Sprintf("%d|%s", h.PID, h.Origin)
	r.mu.Lock()
	if r.logged == nil {
		r.logged = map[string]bool{}
	}
	seen := r.logged[key]
	r.logged[key] = true
	logf := r.logf
	r.mu.Unlock()
	if seen || logf == nil {
		return
	}
	pid := "no pid recorded"
	if h.PID > 0 {
		pid = fmt.Sprintf("pid %d is not running", h.PID)
	}
	logf("nexus: ignoring %s — the Vite dev server it names is not running (%s, and %s does not answer), so it is treated as absent. Start the dev server, or delete the file.", r.path, pid, h.Origin)
}

// probeClient checks a dev server's origin: no proxy from the environment
// (the dev server is local) and no redirects (a dev server answers for its
// client directly; a redirect means something else holds the port).
var probeClient = &http.Client{
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// probeOrigin reports whether a dev server answers for its Vite client. Only
// a success counts: another server on a reused port answers 404.
//
// A server that accepts the connection but has not answered by the deadline
// counts as alive: that is a dev server busy compiling (a cold load of a big
// app), and reporting it dead would make the reload shim see it stop and
// start again, reloading every open page for nothing. Only a refused or
// unreachable origin, or a wrong answer, is dead.
func probeOrigin(ctx context.Context, clientURL string) bool {
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientURL, nil)
	if err != nil {
		return false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return connected.Load() && errors.Is(err, context.DeadlineExceeded)
	}
	// The status is all that matters; don't pull the whole module.
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func load(path string) (*Hot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var h Hot
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON: %w", path, err)
	}
	if h.Version != Version {
		return nil, fmt.Errorf("%s: schema version %d, this nexus reads version %d — update nexus-vite-plugin and nexus together", path, h.Version, Version)
	}
	if h.Origin == "" {
		return nil, fmt.Errorf("%s: no origin — it was written by an incomplete nexus-vite-plugin", path)
	}
	origin, err := ValidateOrigin(h.Origin)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	h.Origin = origin
	return &h, nil
}

// ValidateOrigin checks a dev-server origin and returns it normalised to
// scheme://host[:port]. The origin is written into pages and requested by
// the server, so only a plain http(s) origin is accepted: a host that is a
// DNS name or an IP literal, no userinfo, no path beyond "/", no query or
// fragment — nothing that could carry markup into an attribute.
func ValidateOrigin(origin string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("invalid origin %q: %s", origin, why)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return bad("not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return bad("the scheme must be http or https")
	case u.Opaque != "" || u.Host == "":
		return bad("no host")
	case u.User != nil:
		return bad("userinfo is not allowed")
	case u.Path != "" && u.Path != "/", u.RawPath != "":
		return bad("a path is not allowed")
	case u.RawQuery != "" || u.ForceQuery:
		return bad("a query is not allowed")
	case u.Fragment != "" || strings.Contains(origin, "#"):
		return bad("a fragment is not allowed")
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		// No zone: "%" has no business in a page's URLs.
		if _, err := netip.ParseAddr(host); err != nil || strings.Contains(host, "%") {
			return bad("not an IP literal")
		}
	} else {
		if host == "" {
			return bad("no host")
		}
		for _, c := range host {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return bad(fmt.Sprintf("character %q in the host", c))
			}
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return bad("empty port")
	}
	for _, c := range u.Port() {
		if c < '0' || c > '9' {
			return bad("the port is not a number")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}
