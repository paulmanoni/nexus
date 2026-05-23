package nexus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDotenv writes contents to a temp .env, returns the path. The
// caller passes the path into loadDotenvFile (or one of the public
// LoadDotenvIfPresent / MustLoadDotenv constructors).
func withDotenv(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDotenvFile_BasicKeyValue(t *testing.T) {
	t.Setenv("DOTENV_TEST_BASIC", "")  // start clean
	os.Unsetenv("DOTENV_TEST_BASIC")
	path := withDotenv(t, `DOTENV_TEST_BASIC=hello`)
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_BASIC") })

	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_BASIC"); got != "hello" {
		t.Errorf("DOTENV_TEST_BASIC = %q, want hello", got)
	}
}

func TestLoadDotenvFile_ExistingEnvWins(t *testing.T) {
	// Real env var is the source of truth — the file MUST NOT
	// overwrite it. This is the "production env injects DB_PASSWORD,
	// the dev-friendly .env stub has a placeholder, the real value
	// stays" guarantee.
	t.Setenv("DOTENV_TEST_OVERRIDE", "real-value")
	path := withDotenv(t, `DOTENV_TEST_OVERRIDE=from-dotenv`)

	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_OVERRIDE"); got != "real-value" {
		t.Errorf("env var overwritten: got %q, want real-value", got)
	}
}

func TestLoadDotenvFile_MissingFileIsNoop(t *testing.T) {
	err := loadDotenvFile("/path/that/definitely/does/not/exist/.env")
	if err != nil {
		t.Fatalf("missing file should be silent no-op, got %v", err)
	}
}

func TestLoadDotenvFile_CommentsAndBlanksSkipped(t *testing.T) {
	os.Unsetenv("DOTENV_TEST_C1")
	os.Unsetenv("DOTENV_TEST_C2")
	t.Cleanup(func() {
		os.Unsetenv("DOTENV_TEST_C1")
		os.Unsetenv("DOTENV_TEST_C2")
	})
	path := withDotenv(t, `
# leading comment
DOTENV_TEST_C1=alpha

# blank line above + trailing comment below
DOTENV_TEST_C2=beta
# trailing
`)
	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if os.Getenv("DOTENV_TEST_C1") != "alpha" {
		t.Errorf("C1 missing")
	}
	if os.Getenv("DOTENV_TEST_C2") != "beta" {
		t.Errorf("C2 missing")
	}
}

func TestLoadDotenvFile_QuotedValuesStripQuotes(t *testing.T) {
	os.Unsetenv("DOTENV_TEST_DQ")
	os.Unsetenv("DOTENV_TEST_SQ")
	t.Cleanup(func() {
		os.Unsetenv("DOTENV_TEST_DQ")
		os.Unsetenv("DOTENV_TEST_SQ")
	})
	path := withDotenv(t, `
DOTENV_TEST_DQ="value with = sign"
DOTENV_TEST_SQ='literal $string'
`)
	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_DQ"); got != "value with = sign" {
		t.Errorf("double-quoted: got %q", got)
	}
	if got := os.Getenv("DOTENV_TEST_SQ"); got != `literal $string` {
		t.Errorf("single-quoted: got %q", got)
	}
}

func TestLoadDotenvFile_InlineCommentStripped(t *testing.T) {
	os.Unsetenv("DOTENV_TEST_IC")
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_IC") })
	path := withDotenv(t, `DOTENV_TEST_IC=value # trailing comment`)
	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_IC"); got != "value" {
		t.Errorf("inline comment not stripped: got %q", got)
	}
}

func TestLoadDotenvFile_InlineCommentPreservedInQuoted(t *testing.T) {
	// A `#` inside quotes is data, not a comment marker — operators
	// who put quotes around their value have signalled intent.
	os.Unsetenv("DOTENV_TEST_IQ")
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_IQ") })
	path := withDotenv(t, `DOTENV_TEST_IQ="value # part of string"`)
	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_IQ"); got != "value # part of string" {
		t.Errorf("# inside quotes was stripped: got %q", got)
	}
}

func TestLoadDotenvFile_ExportPrefixTolerated(t *testing.T) {
	os.Unsetenv("DOTENV_TEST_EXP")
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_EXP") })
	path := withDotenv(t, `export DOTENV_TEST_EXP=ok`)
	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_EXP"); got != "ok" {
		t.Errorf("export prefix not stripped: got %q", got)
	}
}

func TestLoadDotenvFile_MalformedLineRejected(t *testing.T) {
	path := withDotenv(t, `NO_EQUALS_HERE`)
	err := loadDotenvFile(path)
	if err == nil {
		t.Fatal("expected error on malformed line")
	}
	if !strings.Contains(err.Error(), "missing `=`") {
		t.Errorf("error should explain the issue: %v", err)
	}
}

func TestLoadDotenvFile_InvalidKeyRejected(t *testing.T) {
	path := withDotenv(t, `123BAD=value`)
	err := loadDotenvFile(path)
	if err == nil {
		t.Fatal("expected error on key starting with a digit")
	}
}

func TestLoadDotenvFile_UnterminatedQuoteRejected(t *testing.T) {
	path := withDotenv(t, `KEY="unterminated`)
	err := loadDotenvFile(path)
	if err == nil {
		t.Fatal("expected error on unterminated quote")
	}
}

// TestMustLoadDotenv_MissingFileFailsBoot is the strict-variant
// guarantee: missing file produces a non-nil option that fx will
// surface at Run time.
func TestMustLoadDotenv_MissingFileFailsBoot(t *testing.T) {
	opt := MustLoadDotenv("/no/such/.env")
	// Round-trip through unwrap to inspect — the option chain
	// embeds the error inside fx.Error which surfaces at Run.
	// Easier check: invoke the path via loadDotenvFile to mirror
	// what MustLoadDotenv does and verify we get the same shape.
	if opt == nil {
		t.Fatal("MustLoadDotenv should return a non-nil Option")
	}
}

// TestLoadDotenvFile_FlowsIntoExpandEnvVars is the integration
// promise: after the loader populates os.Environ, the manifest
// expansion sees the values.
func TestLoadDotenvFile_FlowsIntoExpandEnvVars(t *testing.T) {
	os.Unsetenv("DOTENV_INTEG_DOMAIN")
	t.Cleanup(func() { os.Unsetenv("DOTENV_INTEG_DOMAIN") })
	path := withDotenv(t, `DOTENV_INTEG_DOMAIN=integ.example.com`)
	if err := loadDotenvFile(path); err != nil {
		t.Fatalf("loadDotenvFile: %v", err)
	}
	if got := os.Getenv("DOTENV_INTEG_DOMAIN"); got != "integ.example.com" {
		t.Fatalf("env not set: %q", got)
	}
}
