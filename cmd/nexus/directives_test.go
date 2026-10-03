package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func resetLegacyDirectives() {
	legacyDirectives.Lock()
	defer legacyDirectives.Unlock()
	legacyDirectives.found = map[string]string{}
	legacyDirectives.reported = map[string]bool{}
}

// v1.80 reads both spellings: //nexus:x (2.0's) and //@x (v1's), and the
// two generate the same registration.
func TestScanHandlerSites_BothPrefixes(t *testing.T) {
	resetLegacyDirectives()
	t.Cleanup(resetLegacyDirectives)

	gen := func(src string) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "h.go"), src)
		results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("results = %d", len(results))
		}
		return string(results[0].Content)
	}
	directive := gen(`// Package users serves users.
//
//nexus:path /api
package users

// ListUsers lists users.
//
//nexus:query
//nexus:auth Requires view_user
func ListUsers() {}
`)
	legacy := gen(`// Package users serves users.
//
//@path /api
package users

// ListUsers lists users.
//
// @query
//@auth Requires view_user
func ListUsers() {}
`)
	if directive != legacy {
		t.Fatalf("spellings generate different code:\n--- //nexus:\n%s\n--- //@\n%s", directive, legacy)
	}
	for _, want := range []string{"nexus.AsQuery(ListUsers", `auth.Requires("view_user")`, `nexus.Path("/api")`} {
		if !strings.Contains(directive, want) {
			t.Fatalf("missing %s in:\n%s", want, directive)
		}
	}

	// Only the //@ lines are recorded, once each across rescans.
	var buf bytes.Buffer
	reportLegacyDirectives(&buf)
	out := buf.String()
	if !strings.Contains(out, "3 annotation(s)") || !strings.Contains(out, "//@query → //nexus:query") ||
		!strings.Contains(out, "h.go:3: //@path → //nexus:path") {
		t.Fatalf("report:\n%s", out)
	}
	buf.Reset()
	reportLegacyDirectives(&buf)
	if buf.Len() != 0 {
		t.Fatalf("reported twice:\n%s", buf.String())
	}
}

func TestScanHandlerSites_DirectiveSpelling(t *testing.T) {
	resetLegacyDirectives()
	t.Cleanup(resetLegacyDirectives)
	scan := func(src string) error {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "h.go"), src)
		_, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
		return err
	}

	if err := scan("package h\n\n// nexus:rest GET /users\nfunc List() {}\n"); err == nil ||
		!strings.Contains(err.Error(), "not a Go directive") || !strings.Contains(err.Error(), "h.go:3:") {
		t.Fatalf("spaced //nexus: keyword should fail with file:line, got %v", err)
	}
	if err := scan("package h\n\n//nexus:qurey\nfunc List() {}\n"); err == nil ||
		!strings.Contains(err.Error(), "did you mean //nexus:query") {
		t.Fatalf("unknown //nexus: keyword should fail, got %v", err)
	}
	if err := scan("package h\n\n//nexus:frobnicate\nfunc List() {}\n"); err == nil ||
		!strings.Contains(err.Error(), "unknown annotation //nexus:frobnicate") {
		t.Fatalf("unknown //nexus: keyword should fail, got %v", err)
	}
	// Prose that starts with "nexus:" and another tool's @-annotation are
	// not nexus's.
	if err := scan("package h\n\n// nexus: the framework this handler belongs to.\n// @Summary lists users\nfunc List() {}\n"); err != nil {
		t.Fatalf("prose / swag annotation rejected: %v", err)
	}
}

func TestTypeSites_BothPrefixes(t *testing.T) {
	resetLegacyDirectives()
	t.Cleanup(resetLegacyDirectives)
	gen := func(prefix string) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "c.go"), `package users

// UsersController serves users.
//
`+prefix+`controller /users
`+prefix+`auth Required
type UsersController struct{}

`+prefix+`rest GET /
func (c *UsersController) Index() error { return nil }
`)
		results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
		if err != nil {
			t.Fatalf("scan %s: %v", prefix, err)
		}
		return string(results[0].Content)
	}
	directive, legacy := gen("//nexus:"), gen("//@")
	if directive != legacy || !strings.Contains(directive, "nexus.Controller[*UsersController]") {
		t.Fatalf("type directives differ or missing:\n%s\n---\n%s", directive, legacy)
	}
}
