package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNexusIgnoreMatching(t *testing.T) {
	root := "/app"
	m := parseNexusIgnore(root, strings.NewReader(`
# generated trees and scratch space
generated
tmp/
internal/mock/*.go
assets/**/snapshots
!internal/mock/keep.go
`))
	if m == nil {
		t.Fatal("parse returned nil for a file with patterns")
	}
	if m.patterns() != 5 {
		t.Errorf("patterns() = %d, want 5", m.patterns())
	}

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"generated", true, true},                     // unanchored, matches at the root
		{"api/generated", true, true},                 // ...and at any depth
		{"api/generated/schema.go", false, true},      // everything under it
		{"generatedX", true, false},                   // no partial-name matches
		{"tmp", true, true},                           // trailing slash: directory
		{"tmp", false, false},                         // ...so a FILE named tmp is kept
		{"internal/mock/user.go", false, true},        // anchored glob
		{"internal/mock/keep.go", false, false},       // re-included by the later !rule
		{"other/internal/mock/user.go", false, false}, // anchored: only from the root
		{"assets/a/b/snapshots", true, true},          // ** spans directories
		{"assets/a/b/snapshots/one.png", false, true}, // and their contents
		{"assets/snapshots", true, true},              // ** also spans zero
		{"cmd/app/main.go", false, false},             // ordinary source is untouched
		{"../outside/main.go", false, false},          // outside the root never matches
	}
	for _, c := range cases {
		got := m.match(filepath.Join(root, c.path), c.isDir)
		if c.path == "../outside/main.go" {
			got = m.match("/outside/main.go", c.isDir)
		}
		if got != c.want {
			t.Errorf("match(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestNexusIgnoreEmptyAndComments(t *testing.T) {
	if m := parseNexusIgnore("/app", strings.NewReader("\n# only comments\n\n")); m != nil {
		t.Errorf("expected nil matcher for a file with no patterns, got %d", m.patterns())
	}
	// A nil matcher is the common case and must be safe to call.
	var m *ignoreMatcher
	if m.match("/app/main.go", false) {
		t.Error("nil matcher ignored a path")
	}
	if m.patterns() != 0 {
		t.Error("nil matcher reported patterns")
	}
}

func TestLoadNexusIgnoreMissingFile(t *testing.T) {
	if m := loadNexusIgnore(t.TempDir()); m != nil {
		t.Error("expected nil matcher when .nexusignore is absent")
	}
}
