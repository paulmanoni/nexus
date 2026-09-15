package nexus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigError_MissingEnvVar walks the real load path: a nexus.toml
// referencing an unset ${VAR} must classify into a ConfigError carrying
// the file, the 1-based line, the variable, and a suggested fix.
func TestConfigError_MissingEnvVar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nexus.toml")
	body := `[runtime]
environment = "production"

[databases.main]
password = "${DEFINITELY_NOT_SET_VAR}"
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatal("want error for unset env var")
	}
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ConfigError, got %T: %v", err, err)
	}
	if ce.Source != p {
		t.Fatalf("Source = %q, want %q", ce.Source, p)
	}
	if ce.Line != 5 {
		t.Fatalf("Line = %d, want 5", ce.Line)
	}
	if !strings.Contains(ce.Err.Error(), "DEFINITELY_NOT_SET_VAR is not set") {
		t.Fatalf("cause = %q", ce.Err)
	}
	if !strings.Contains(ce.Hint, "${DEFINITELY_NOT_SET_VAR:default}") {
		t.Fatalf("hint = %q", ce.Hint)
	}
}

// TestConfigError_ParseError pins that a TOML syntax error carries the
// row and go-toml's annotated snippet.
func TestConfigError_ParseError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(p, []byte("[runtime]\naddr = :8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(p)
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ConfigError, got %T: %v", err, err)
	}
	if ce.Line != 2 {
		t.Fatalf("Line = %d, want 2", ce.Line)
	}
	if ce.Snippet == "" {
		t.Fatal("want go-toml's annotated snippet")
	}
}

// TestRenderBootError_Layout locks the operator-facing shape: a title
// line, aligned file/error/fix rows, no goroutine dump — and ANSI codes
// only when color is on.
func TestRenderBootError_Layout(t *testing.T) {
	ce := &ConfigError{
		Source: "/app/nexus.toml",
		Stage:  "expand env vars",
		Line:   139,
		Err:    errors.New("env var OATS_DB_PASSWORD is not set"),
		Hint:   "export OATS_DB_PASSWORD=…",
	}

	var plain strings.Builder
	renderBootError(&plain, ce, false)
	out := plain.String()
	for _, want := range []string{
		"✗ nexus: cannot load config",
		"file",
		"/app/nexus.toml:139",
		"error",
		"env var OATS_DB_PASSWORD is not set",
		"fix",
		"export OATS_DB_PASSWORD=…",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain output must carry no ANSI codes:\n%q", out)
	}
	if strings.Contains(out, "goroutine") {
		t.Fatalf("output must not look like a panic:\n%s", out)
	}

	var colored strings.Builder
	renderBootError(&colored, ce, true)
	if !strings.Contains(colored.String(), "\x1b[31m") {
		t.Fatalf("colored output missing red ANSI code:\n%q", colored.String())
	}

	// Non-config errors get the generic block, not a crash dump.
	var generic strings.Builder
	renderBootError(&generic, errors.New("listen tcp :8080: address already in use"), false)
	if !strings.Contains(generic.String(), "✗ nexus: failed to start") {
		t.Fatalf("generic block missing title:\n%s", generic.String())
	}
}
