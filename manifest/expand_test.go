package manifest

import (
	"strings"
	"testing"
)

// TestExpandEnvVars_BasicSubstitution is the happy path: a single
// ${VAR} inside a quoted string is replaced with the env value.
func TestExpandEnvVars_BasicSubstitution(t *testing.T) {
	t.Setenv("APP_DOMAIN", "app.example.com")
	in := []byte(`
[environments.production]
domain = "${APP_DOMAIN}"
`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `domain = "app.example.com"`) {
		t.Errorf("missing substitution:\n%s", out)
	}
}

// TestExpandEnvVars_DefaultWhenUnset proves the fallback path —
// ${VAR:default} resolves to the literal default when the env var
// is unset.
func TestExpandEnvVars_DefaultWhenUnset(t *testing.T) {
	in := []byte(`ttl = "${APP_TTL_UNSET:7d}"`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `ttl = "7d"`) {
		t.Errorf("missing default:\n%s", out)
	}
}

// TestExpandEnvVars_DefaultWhenEmpty mirrors bash's `${X:-d}` —
// an exported-but-empty env var counts as "unset" so the default
// kicks in. Catches the "deploy script exported the variable but
// the value collapsed to empty" failure mode.
func TestExpandEnvVars_DefaultWhenEmpty(t *testing.T) {
	t.Setenv("APP_TTL", "")
	in := []byte(`ttl = "${APP_TTL:7d}"`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `ttl = "7d"`) {
		t.Errorf("empty env var should fall through to default:\n%s", out)
	}
}

// TestExpandEnvVars_StrictModeRejectsUnset is the headline strict-
// mode test: an undefined ${VAR} without a default fails the load
// with the variable's name in the error.
func TestExpandEnvVars_StrictModeRejectsUnset(t *testing.T) {
	in := []byte(`domain = "${NOT_DEFINED_ANYWHERE}"`)
	_, err := expandEnvVars(in)
	if err == nil {
		t.Fatal("expected strict-mode error on undefined env var")
	}
	if !strings.Contains(err.Error(), "NOT_DEFINED_ANYWHERE") {
		t.Errorf("error should name the variable: %v", err)
	}
}

// TestExpandEnvVars_NestedDefault exercises the one-level nested
// fallback: ${OUTER:${INNER:c}} drops through OUTER → INNER → c.
func TestExpandEnvVars_NestedDefault(t *testing.T) {
	in := []byte(`cdn = "${CDN_HOST:${APP_DOMAIN:localhost}}"`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `cdn = "localhost"`) {
		t.Errorf("nested fallback failed:\n%s", out)
	}
}

// TestExpandEnvVars_NestedResolvesFirstHit checks that the nested
// resolver short-circuits on the first set variable — APP_DOMAIN
// wins over the deepest fallback.
func TestExpandEnvVars_NestedResolvesFirstHit(t *testing.T) {
	t.Setenv("APP_DOMAIN", "app.example.com")
	in := []byte(`cdn = "${CDN_HOST:${APP_DOMAIN:localhost}}"`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `cdn = "app.example.com"`) {
		t.Errorf("nested resolver should pick APP_DOMAIN:\n%s", out)
	}
}

// TestExpandEnvVars_Escape proves the $${X} escape leaves a literal
// ${X} in the output — useful for operators who want to reference
// the syntax in a comment or string without expansion.
func TestExpandEnvVars_Escape(t *testing.T) {
	in := []byte(`note = "use $${SOMETHING} syntax"`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `note = "use ${SOMETHING} syntax"`) {
		t.Errorf("escape failed:\n%s", out)
	}
}

// TestExpandEnvVars_LiteralStringsAreSkipped: TOML literal strings
// ('...' / '''...''') are raw by spec, so ${X} inside them stays
// as-is. Gives operators a no-expansion escape hatch without needing
// the $$ trick.
func TestExpandEnvVars_LiteralStringsAreSkipped(t *testing.T) {
	t.Setenv("FOO", "bar")
	in := []byte(`
literal = '${FOO}'
basic   = "${FOO}"
`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `literal = '${FOO}'`) {
		t.Errorf("literal string was expanded (should not be):\n%s", s)
	}
	if !strings.Contains(s, `basic   = "bar"`) {
		t.Errorf("basic string not expanded:\n%s", s)
	}
}

// TestExpandEnvVars_CommentsAreSkipped: # comments pass through
// untouched, so an operator can document ${X} syntax without
// tripping the expander.
func TestExpandEnvVars_CommentsAreSkipped(t *testing.T) {
	in := []byte(`
# Use ${SOME_VAR} for runtime injection
key = "value"
`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(string(out), `# Use ${SOME_VAR} for runtime injection`) {
		t.Errorf("comment was rewritten:\n%s", out)
	}
}

// TestExpandEnvVars_NonStringScalarsUntouched: raw integers,
// booleans, and table headers are passed through verbatim — only
// basic-string values get the expansion treatment.
func TestExpandEnvVars_NonStringScalarsUntouched(t *testing.T) {
	in := []byte(`
[environments.production]
port = 8080
ssl  = true
`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("non-string content was modified:\nwant: %q\n got: %q", in, out)
	}
}

// TestExpandEnvVars_EscapedValuePreservesTOMLSyntax: when the env
// value contains a `"` or `\`, those bytes must be escaped before
// re-emission or the TOML parser would mis-tokenize the surrounding
// string. Guards against `domain = "${X}"` blowing up when X
// contains a quote.
func TestExpandEnvVars_EscapedValuePreservesTOMLSyntax(t *testing.T) {
	t.Setenv("WITH_QUOTE", `a"b`)
	t.Setenv("WITH_BACKSLASH", `a\b`)
	in := []byte(`
q = "${WITH_QUOTE}"
b = "${WITH_BACKSLASH}"
`)
	out, err := expandEnvVars(in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `q = "a\"b"`) {
		t.Errorf("quote not escaped:\n%s", s)
	}
	if !strings.Contains(s, `b = "a\\b"`) {
		t.Errorf("backslash not escaped:\n%s", s)
	}
}

// TestExpandEnvVars_ParsesThroughLoadInputsTOML proves the
// expansion wires into the real loader: a manifest using ${VAR}
// round-trips into the typed Manifest with the expanded value.
func TestExpandEnvVars_ParsesThroughLoadInputsTOML(t *testing.T) {
	t.Setenv("APP_DOMAIN", "prod.example.com")
	doc := []byte(`
[environments.production]
domain = "${APP_DOMAIN}"
ttl    = "${APP_TTL:7d}"
`)
	m, err := LoadInputsTOML(doc)
	if err != nil {
		t.Fatalf("LoadInputsTOML: %v", err)
	}
	if len(m.Environments) != 1 {
		t.Fatalf("environments: %+v", m.Environments)
	}
	if m.Environments[0].Domain != "prod.example.com" {
		t.Errorf("domain: got %q, want prod.example.com", m.Environments[0].Domain)
	}
	if m.Environments[0].TTL != "7d" {
		t.Errorf("ttl: got %q, want 7d (default)", m.Environments[0].TTL)
	}
}

// TestExpandEnvVars_LineNumberInError: a strict-mode failure points
// at the offending line so operators can locate it in $EDITOR.
func TestExpandEnvVars_LineNumberInError(t *testing.T) {
	in := []byte(`# header
key1 = "value"
key2 = "${MISSING_THING}"
`)
	_, err := expandEnvVars(in)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should mention line 3: %v", err)
	}
}

// TestExpandEnvVars_EmptyVariableNameRejected guards against ${}
// silently slipping through — the loader should refuse instead of
// producing a confusing downstream parse error.
func TestExpandEnvVars_EmptyVariableNameRejected(t *testing.T) {
	in := []byte(`key = "${}"`)
	_, err := expandEnvVars(in)
	if err == nil {
		t.Fatal("expected error on empty ${} token")
	}
}

// TestExpandEnvVars_UnterminatedTokenRejected catches a typo where
// the operator forgets the closing brace.
func TestExpandEnvVars_UnterminatedTokenRejected(t *testing.T) {
	in := []byte(`key = "${UNCLOSED"`)
	_, err := expandEnvVars(in)
	if err == nil {
		t.Fatal("expected error on unterminated ${ token")
	}
}
