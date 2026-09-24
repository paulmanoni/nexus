// Package vitemanifest reads the manifest `vite build` writes — the prod half
// of the frontend contract, as internal/vitehot is the dev half. It is the one
// parser for it: inertia turns it into asset tags, ServeFrontend into cache
// policy. nexus-vite-plugin forces build.manifest on, so an app built with the
// plugin always has one.
package vitemanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// Chunk is one record of a Vite build manifest.
type Chunk struct {
	File           string   `json:"file"`           // the emitted file, e.g. "assets/main-ab12cd34.js"
	Src            string   `json:"src"`            // the source it was built from
	CSS            []string `json:"css"`            // stylesheets emitted for this chunk
	Assets         []string `json:"assets"`         // other files it references (images, fonts)
	Imports        []string `json:"imports"`        // manifest keys of statically-imported chunks
	DynamicImports []string `json:"dynamicImports"` // manifest keys of lazily-imported chunks
	IsEntry        bool     `json:"isEntry"`        // an entry point of the build
}

// Manifest is a parsed build manifest.
type Manifest struct {
	Chunks map[string]Chunk
	// Path is the file that was read, relative to the fs it came from.
	Path string
	// Version is a short hash of the raw bytes. Any change to any emitted
	// file changes it, which makes it a ready-made asset version.
	Version string
}

// Candidates are the locations Load tries under root, in order: where Vite 5+
// writes the manifest, then the top-level location older Vite used.
func Candidates(root string) []string {
	return []string{
		path.Join(root, ".vite", "manifest.json"),
		path.Join(root, "manifest.json"),
	}
}

// Load reads the manifest under root. When none exists the error wraps
// fs.ErrNotExist, so a caller can tell "no build" from "a broken one".
func Load(fsys fs.FS, root string) (*Manifest, error) {
	var tried []string
	for _, p := range Candidates(root) {
		raw, err := fs.ReadFile(fsys, p)
		if errors.Is(err, fs.ErrNotExist) {
			tried = append(tried, p)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var chunks map[string]Chunk
		if err := json.Unmarshal(raw, &chunks); err != nil {
			return nil, fmt.Errorf("%s is not a valid Vite manifest: %w", p, err)
		}
		sum := sha256.Sum256(raw)
		return &Manifest{Chunks: chunks, Path: p, Version: hex.EncodeToString(sum[:])[:16]}, nil
	}
	return nil, fmt.Errorf("no Vite manifest (tried %s): %w", strings.Join(tried, ", "), fs.ErrNotExist)
}

// EntryKey is the key of the entry chunk. With several, the lexically
// smallest, so the choice does not depend on map order. "" when there is none.
func (m *Manifest) EntryKey() string {
	key := ""
	for k, c := range m.Chunks {
		if c.IsEntry && c.File != "" && (key == "" || k < key) {
			key = k
		}
	}
	return key
}

// hashedName matches Vite's content-hash suffix, "[name]-[hash].ext" by
// default: a dash and at least eight hash characters before the extension.
var hashedName = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

// IsHashedName reports whether a file name carries a content hash — the
// property that makes it safe to cache forever, since new content gets a new
// name.
func IsHashedName(p string) bool { return hashedName.MatchString(path.Base(p)) }

// Immutable returns the emitted files that can be cached forever: every file
// the manifest lists as build output whose name also carries a content hash.
// Both conditions are needed — the manifest proves Vite produced the file,
// and a config such as entryFileNames: "[name].js" produces output with no
// hash, which must not be cached as if it could never change. Paths are
// relative to the build output directory.
func (m *Manifest) Immutable() map[string]bool {
	out := map[string]bool{}
	add := func(p string) {
		if p != "" && IsHashedName(p) {
			out[strings.TrimPrefix(p, "/")] = true
		}
	}
	for _, c := range m.Chunks {
		add(c.File)
		for _, p := range c.CSS {
			add(p)
		}
		for _, p := range c.Assets {
			add(p)
		}
	}
	return out
}
