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
// Enabled says the app is in development.
package vitehot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// Enabled is the single rule for when a hot file may be honoured: under
// `nexus dev`, or when the app declares environment = "development". The
// runtime environment defaults to "production", so a binary that says nothing
// never follows a hot file — a stale one left on disk cannot point a
// production page at a dev server that is not there.
func Enabled(isDev bool, environment string) bool {
	return isDev || strings.EqualFold(environment, "development")
}

// Hot is the decoded hot file.
type Hot struct {
	Version int      `json:"version"`
	Origin  string   `json:"origin"`
	Base    string   `json:"base"`
	Entries []string `json:"entries"`
	PID     int      `json:"pid"`
}

// URL resolves a path served by the dev server, honouring Vite's base.
func (h *Hot) URL(p string) string {
	base := h.Base
	// A full-URL base (a CDN) only applies to built assets; Vite ignores its
	// origin in development and serves under its path.
	if u, err := url.Parse(base); err == nil && u.IsAbs() {
		base = u.Path
	}
	if base == "" {
		base = "/"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return strings.TrimRight(h.Origin, "/") + base + strings.TrimPrefix(p, "/")
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
		if e != "" && !strings.HasSuffix(strings.ToLower(e), ".html") {
			return e
		}
	}
	return ""
}

// Reader returns the current hot file. It re-reads the file on every call:
// the file is a few hundred bytes and is consulted only when rendering a page
// in development, and caching on mtime and size would miss a same-length
// rewrite (port 5173 → 5174) landing within one timestamp tick. Re-reading is
// what makes a Vite restart on a different port apply on the next request,
// with no Go restart — the env-var mechanism it replaces was read once per
// process.
type Reader struct {
	path    string
	enabled func() bool
}

// NewReader watches the hot file for a build output directory on disk.
// enabled is consulted on every call; pass a closure over Enabled.
func NewReader(distDir string, enabled func() bool) *Reader {
	p := Path(distDir)
	// Absolute, so an error message tells the developer exactly which file,
	// whatever directory they are reading it from.
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return &Reader{path: p, enabled: enabled}
}

// Path is the file this reader watches — for messages that tell a developer
// where to look.
func (r *Reader) Path() string { return r.path }

// ErrStale reports a hot file whose writing process has exited without
// removing it, which is what a killed `vite dev` leaves behind.
var ErrStale = errors.New("vite dev server that wrote it is no longer running")

// Current returns the hot file when one is present and usable. (nil, nil)
// means there is no dev server to use: the reader is disabled, or the file is
// absent. A non-nil error means a file IS present but cannot be used —
// malformed, an unknown schema version, or left behind by a dead process —
// and callers should say so rather than silently fall back.
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
	// The file does not change when its writer dies, so only the pid can say
	// whether the dev server it describes is still there.
	if h.PID > 0 && !alive(h.PID) {
		return nil, fmt.Errorf("%s: %w (pid %d) — restart it, or delete the file", r.path, ErrStale, h.PID)
	}
	return h, nil
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
	return &h, nil
}
