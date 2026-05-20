package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// packageJSON is the minimal package.json shape nexus reads and
// writes. We intentionally model only the fields we care about
// (name/type/private/dependencies); any extra fields the user
// hand-adds are preserved verbatim by round-tripping through a
// generic map, then re-merging on save.
//
// Why we touch package.json at all: the file is recognized by every
// JS-aware tool (IDEs, Renovate, Dependabot, language servers) as
// "this is a JS project, here are its deps." nexus.lock is the
// authoritative pin file, but it's an internal format — without
// package.json the user's tooling can't see what the project depends
// on, can't suggest dep updates, can't autocomplete. We let
// package.json be the human-facing spec and nexus.lock be the
// machine-facing resolution.
//
// The flow mirrors npm/pnpm:
//
//	nexus add <spec>         → package.json gets {<spec>: "^<ver>"}
//	                           and nexus.lock gets the exact pin
//	nexus remove <spec>      → both files lose the entry
//	nexus install            → reads package.json, fetches missing,
//	                           verifies integrity against nexus.lock
//
// We DON'T use the "scripts" / "devDependencies" / "engines"
// fields — nexus is not a npm runner. But we preserve them if the
// user adds them manually.
type packageJSON struct {
	// extras holds any field we don't model explicitly so they
	// survive round-trips. Saved back verbatim with the same key.
	extras map[string]json.RawMessage

	Name         string
	Type         string
	Private      bool
	Dependencies map[string]string
}

// packageJSONFilename is the standard name. Always at the project
// root next to nexus.lock and go.mod.
const packageJSONFilename = "package.json"

// loadPackageJSON reads the package.json at path. Returns a
// zero-deps struct when the file doesn't exist so callers can
// always proceed to AddDep without a nil check; the user's first
// `nexus add` will create the file on Save.
func loadPackageJSON(path string) (*packageJSON, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &packageJSON{
			Dependencies: map[string]string{},
			extras:       map[string]json.RawMessage{},
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("package.json: read %s: %w", path, err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("package.json: parse %s: %w", path, err)
	}
	out := &packageJSON{
		Dependencies: map[string]string{},
		extras:       map[string]json.RawMessage{},
	}
	for k, v := range generic {
		switch k {
		case "name":
			_ = json.Unmarshal(v, &out.Name)
		case "type":
			_ = json.Unmarshal(v, &out.Type)
		case "private":
			_ = json.Unmarshal(v, &out.Private)
		case "dependencies":
			_ = json.Unmarshal(v, &out.Dependencies)
		default:
			out.extras[k] = v
		}
	}
	if out.Dependencies == nil {
		out.Dependencies = map[string]string{}
	}
	return out, nil
}

// save writes package.json with deterministic formatting (sorted
// keys, 2-space indent, LF newlines, trailing newline) so commits
// have minimal diffs and review tooling stays happy. Atomic write
// via temp+rename so a crashed save never leaves the file half-
// written.
//
// Field ordering matches npm/pnpm convention: name → type →
// private → dependencies → everything else alphabetized. This is
// the order most JS devs expect to see, even though JSON itself
// has no ordering — keeping it consistent avoids unnecessary diffs
// when nexus rewrites a file that npm or pnpm wrote.
func (p *packageJSON) save(path string) error {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	written := 0
	write := func(key string, raw []byte) {
		if written > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString("  ")
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteString(": ")
		buf.Write(raw)
		written++
	}
	emit := func(key string, v any) error {
		raw, err := marshalIndented(v, 2)
		if err != nil {
			return fmt.Errorf("package.json: marshal %s: %w", key, err)
		}
		write(key, raw)
		return nil
	}
	if p.Name != "" {
		if err := emit("name", p.Name); err != nil {
			return err
		}
	}
	if p.Type != "" {
		if err := emit("type", p.Type); err != nil {
			return err
		}
	}
	if p.Private {
		if err := emit("private", p.Private); err != nil {
			return err
		}
	}
	if len(p.Dependencies) > 0 {
		raw, err := marshalSortedMap(p.Dependencies, 2)
		if err != nil {
			return err
		}
		write("dependencies", raw)
	}
	// Extras last, alphabetized so the diff is stable. Skip keys we
	// own explicitly (they're already written above).
	owned := map[string]bool{
		"name":         true,
		"type":         true,
		"private":      true,
		"dependencies": true,
	}
	keys := make([]string, 0, len(p.extras))
	for k := range p.extras {
		if owned[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, p.extras[k])
	}
	buf.WriteString("\n}\n")

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("package.json: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("package.json: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("package.json: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("package.json: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("package.json: rename: %w", err)
	}
	return nil
}

// addDep registers spec at version. Idempotent: a second call with
// the same spec overwrites the version (a `nexus add vue@3.5.0`
// after an earlier `nexus add vue@3.4.0` should reflect the newer
// pin). Mirrors npm's caret-prefix convention so hand-edits with
// `^3.4.0` to allow patch upgrades remain visually consistent with
// nexus-generated entries.
func (p *packageJSON) addDep(spec, version string) {
	if p.Dependencies == nil {
		p.Dependencies = map[string]string{}
	}
	// Bare version → prefix with ^ so the file reads like a normal
	// package.json. Exact pinning (no prefix) is preserved when the
	// caller passes "=3.4.0" or similar explicit range.
	if version != "" && !strings.ContainsAny(version[:1], "^~>=<") {
		version = "^" + version
	}
	p.Dependencies[spec] = version
}

// removeDep deletes spec from dependencies. Returns true if a
// deletion happened — the CLI prints a different message when the
// remove was a no-op vs. an actual change.
func (p *packageJSON) removeDep(spec string) bool {
	if p.Dependencies == nil {
		return false
	}
	if _, ok := p.Dependencies[spec]; !ok {
		return false
	}
	delete(p.Dependencies, spec)
	return true
}

// marshalIndented is json.Marshal but with the standard 2-space
// indent applied to nested structures. Used so primitive values
// (string, bool) emit as their compact form, while objects /
// arrays get nicely indented at the caller's chosen base indent.
func marshalIndented(v any, baseIndent int) ([]byte, error) {
	switch v.(type) {
	case string, bool, int, int64, float64:
		return json.Marshal(v)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(strings.Repeat(" ", baseIndent), "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a trailing newline; trim it so the value can
	// be embedded in our outer JSON object cleanly.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalSortedMap renders a string→string map with keys sorted
// lexicographically. Avoids the random ordering encoding/json
// applies to plain maps (deterministic encoding is std-lib promise
// for string-keyed maps, but we sort explicitly so the contract
// doesn't depend on which Go version compiled nexus).
func marshalSortedMap(m map[string]string, baseIndent int) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	indent := strings.Repeat(" ", baseIndent+2)
	for i, k := range keys {
		buf.WriteString(indent)
		kj, _ := json.Marshal(k)
		buf.Write(kj)
		buf.WriteString(": ")
		vj, _ := json.Marshal(m[k])
		buf.Write(vj)
		if i < len(keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString(strings.Repeat(" ", baseIndent))
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
