package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

// The views compiler's warnings reach the editor as warnings on their file.
func TestWarningDiagnostics(t *testing.T) {
	dir := t.TempDir()
	templ := filepath.Join(dir, "orders.templ")
	ds := warningDiagnostics([]*viewgen.PositionError{{File: filepath.ToSlash(templ), Line: 12, Col: 22, Msg: "o.Total is not a view.Assign"}})
	got := ds[templ]
	if len(got) != 1 || got[0].Severity != 2 || got[0].Range.Start.Line != 11 || got[0].Range.Start.Character != 21 {
		t.Fatalf("diagnostics = %+v", ds)
	}
}

// nexus dev shows warnings when they change, and says when they are gone.
func TestWarningNotes(t *testing.T) {
	root := t.TempDir()
	w := &viewgen.PositionError{File: filepath.Join(root, "a.templ"), Line: 1, Col: 2, Msg: "m"}
	var n warningNotes
	n.set(root, nil)
	if got := n.take(); got != nil {
		t.Fatalf("no warnings, nothing to show: %v", got)
	}
	n.set(root, []*viewgen.PositionError{w})
	if got := n.take(); len(got) != 1 || !strings.HasPrefix(got[0], "a.templ:1:2: m") {
		t.Fatalf("a new warning, relative to the project: %v", got)
	}
	n.set(root, []*viewgen.PositionError{w})
	if got := n.take(); got != nil {
		t.Fatalf("the same warning again: %v", got)
	}
	n.set(root, nil)
	if got := n.take(); len(got) != 1 || got[0] != "warnings resolved" {
		t.Fatalf("warnings gone: %v", got)
	}
}
