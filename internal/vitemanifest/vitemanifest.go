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
	"strings"
)

// Chunk is one record of a Vite build manifest.
type Chunk struct {
	File           string   `json:"file"`           // the emitted file, e.g. "assets/main-ab12cd34.js"
	Name           string   `json:"name"`           // the chunk name [name] expanded to, e.g. "main"
	Names          []string `json:"names"`          // an asset's original file names (Vite 6+), e.g. ["logo.svg"]
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

// hashLen is the length of Vite's default [hash]: eight base64url characters
// (Rollup's hash alphabet includes '-' and '_').
const hashLen = 8

// IsHashedName reports whether a file name carries a content hash in Vite's
// default "[name]-[hash].ext" shape — the property that makes it safe to
// cache forever, since new content gets a new name.
//
// The hash alphabet includes '-', so "a dash, then eight or more
// hash characters" also matches kebab-case words: admin-dashboard.js,
// inter-variable.woff2. A year-long cache cannot be recalled from browsers,
// so the test errs toward "not hashed": the part after the final dash-and-
// eight must be exactly eight base64url characters, and must include a digit
// or an uppercase letter. A real hash made only of lowercase letters is rare
// (about 1 in 1,400) and misclassifying one costs a revalidation, nothing
// more; an English word taken for a hash would be served stale for a year.
func IsHashedName(p string) bool {
	stem, ok := nameStem(path.Base(p))
	if !ok || len(stem) < hashLen+1 || stem[len(stem)-hashLen-1] != '-' {
		return false
	}
	return looksLikeHash(stem[len(stem)-hashLen:], hashLen, hashLen)
}

// nameStem strips the extension. A name with no extension is not Vite
// output shaped [name]-[hash][extname].
func nameStem(base string) (string, bool) {
	ext := path.Ext(base)
	if ext == "" || ext == base {
		return "", false
	}
	return strings.TrimSuffix(base, ext), true
}

// looksLikeHash: min..max base64url characters with at least one digit or
// uppercase letter.
func looksLikeHash(h string, min, max int) bool {
	if len(h) < min || len(h) > max {
		return false
	}
	strong := false
	for i := 0; i < len(h); i++ {
		switch c := h[i]; {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z':
			strong = true
		case c >= 'a' && c <= 'z', c == '-', c == '_':
		default:
			return false
		}
	}
	return strong
}

// hashedByName decides from what the manifest says the file was named
// before [hash] was applied: the chunk name, an asset's original names, the
// source's file name. The emitted stem equal to one of them means no hash —
// definitively, whatever the name looks like (nav-UserCard.js). The stem
// being that name, a dash, and a hash-looking rest means hashed, including a
// longer [hash:N]. ok is false when no known name explains the file.
func hashedByName(file string, names []string) (hashed, ok bool) {
	stem, hasExt := nameStem(path.Base(file))
	if !hasExt {
		return false, false
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		if stem == n {
			return false, true
		}
		if rest, found := strings.CutPrefix(stem, n+"-"); found && looksLikeHash(rest, hashLen, 64) {
			return true, true
		}
	}
	return false, false
}

// knownNames are the pre-hash names a record gives for its own file.
func (c Chunk) knownNames() []string {
	names := []string{c.Name}
	for _, n := range c.Names {
		if s, ok := nameStem(path.Base(n)); ok {
			names = append(names, s)
		}
	}
	if c.Src != "" {
		if s, ok := nameStem(path.Base(c.Src)); ok {
			names = append(names, s)
		}
	}
	return names
}

// Immutable returns the emitted files that can be cached forever: every file
// the manifest lists as build output whose name also carries a content hash.
// Both conditions are needed — the manifest proves Vite produced the file,
// and a config such as entryFileNames: "[name].js" produces output with no
// hash, which must not be cached as if it could never change. Whether a name
// carries a hash is decided from the record's own names where it has them
// (see hashedByName) and from the name's shape otherwise (IsHashedName). Any
// record calling a file unhashed wins. Paths are relative to the build
// output directory.
func (m *Manifest) Immutable() map[string]bool {
	byName := map[string]bool{}
	for _, c := range m.Chunks {
		p := strings.TrimPrefix(c.File, "/")
		if p == "" {
			continue
		}
		if hashed, ok := hashedByName(p, c.knownNames()); ok {
			if prev, seen := byName[p]; seen {
				hashed = hashed && prev
			}
			byName[p] = hashed
		}
	}
	out := map[string]bool{}
	add := func(p string) {
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			return
		}
		hashed, ok := byName[p]
		if !ok {
			hashed = IsHashedName(p)
		}
		if hashed {
			out[p] = true
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
