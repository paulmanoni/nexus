package nexus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/di"
)

// TestExtensionRegistry_RegisterAndLookup: the basic
// registry contract.
func TestExtensionRegistry_RegisterAndLookup(t *testing.T) {
	resetRegistry(t)
	RegisterExtensionDecoder("myext", func(raw []byte) ([]Option, error) {
		return nil, nil
	})
	if LookupExtensionDecoder("myext") == nil {
		t.Errorf("expected myext decoder after register")
	}
	if LookupExtensionDecoder("unknown") != nil {
		t.Errorf("unregistered name should return nil")
	}
}

// TestExtensionRegistry_EmptyInputsIgnored: empty name or nil
// decoder is a no-op — defensive against init() typos that
// would otherwise pollute the registry.
func TestExtensionRegistry_EmptyInputsIgnored(t *testing.T) {
	resetRegistry(t)
	RegisterExtensionDecoder("", func(raw []byte) ([]Option, error) { return nil, nil })
	RegisterExtensionDecoder("nilfn", nil)
	if LookupExtensionDecoder("") != nil {
		t.Errorf("empty name should not register")
	}
	if LookupExtensionDecoder("nilfn") != nil {
		t.Errorf("nil decoder should not register")
	}
}

// TestRegisteredExtensionNames_Sorted: stable alphabetical
// listing for lint output + diagnostic consistency.
func TestRegisteredExtensionNames_Sorted(t *testing.T) {
	resetRegistry(t)
	RegisterExtensionDecoder("zebra", noopDecoder)
	RegisterExtensionDecoder("alpha", noopDecoder)
	RegisterExtensionDecoder("mango", noopDecoder)
	got := RegisteredExtensionNames()
	want := []string{"alpha", "mango", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDecodeExtensions_HappyPath: a [extensions.x] block
// dispatches to the registered decoder and the returned
// Options are collected in alphabetical order.
func TestDecodeExtensions_HappyPath(t *testing.T) {
	resetRegistry(t)

	var calledA, calledB bool
	RegisterExtensionDecoder("a", func(raw []byte) ([]Option, error) {
		calledA = true
		if !strings.Contains(string(raw), "value-a") {
			t.Errorf("a got: %s", raw)
		}
		return []Option{stubOption("opt-a")}, nil
	})
	RegisterExtensionDecoder("b", func(raw []byte) ([]Option, error) {
		calledB = true
		if !strings.Contains(string(raw), "value-b") {
			t.Errorf("b got: %s", raw)
		}
		return []Option{stubOption("opt-b")}, nil
	})

	raw := []byte(`
[extensions.a]
key = "value-a"

[extensions.b]
key = "value-b"
`)
	opts, err := decodeExtensions(raw)
	if err != nil {
		t.Fatalf("decodeExtensions: %v", err)
	}
	if !calledA || !calledB {
		t.Errorf("both decoders should have been called: a=%v b=%v", calledA, calledB)
	}
	if len(opts) != 2 {
		t.Errorf("expected 2 options, got %d", len(opts))
	}
}

// TestDecodeExtensions_UnknownExtensionErrors: an
// [extensions.X] block with no registered decoder is an
// operator error caught at config-load time, not silently
// dropped (which would make typos invisible).
func TestDecodeExtensions_UnknownExtensionErrors(t *testing.T) {
	resetRegistry(t)
	raw := []byte(`
[extensions.unregistered]
key = "value"
`)
	_, err := decodeExtensions(raw)
	if err == nil {
		t.Fatal("expected error for unregistered extension")
	}
	if !strings.Contains(err.Error(), "unregistered") {
		t.Errorf("error should name the unregistered extension, got: %v", err)
	}
}

// TestDecodeExtensions_DecoderErrorPropagates: a malformed
// extension sub-tree fails the WHOLE load — partial Option
// lists would leave the app booted in an unknown state.
func TestDecodeExtensions_DecoderErrorPropagates(t *testing.T) {
	resetRegistry(t)
	RegisterExtensionDecoder("brittle", func(raw []byte) ([]Option, error) {
		return nil, errors.New("endpoint is required")
	})
	raw := []byte(`[extensions.brittle]
key = "value"`)
	_, err := decodeExtensions(raw)
	if err == nil {
		t.Fatal("expected decoder error to propagate")
	}
	if !strings.Contains(err.Error(), "brittle") {
		t.Errorf("error should name the offending extension: %v", err)
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("decoder's message should appear: %v", err)
	}
}

// TestLoadExtensionOptions_FromFile: drives the file-path
// round trip the CLI uses.
func TestLoadExtensionOptions_FromFile(t *testing.T) {
	resetRegistry(t)
	RegisterExtensionDecoder("myext", func(raw []byte) ([]Option, error) {
		return []Option{stubOption("loaded")}, nil
	})
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	if err := os.WriteFile(path, []byte(`
[extensions.myext]
url = "http://example.com"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, err := LoadExtensionOptions(path)
	if err != nil {
		t.Fatalf("LoadExtensionOptions: %v", err)
	}
	if len(opts) != 1 {
		t.Errorf("expected 1 option, got %d", len(opts))
	}
}

// TestLoadExtensionOptions_MissingFileIsOk: no nexus.toml is
// not an error — existing Go-only apps keep working.
func TestLoadExtensionOptions_MissingFileIsOk(t *testing.T) {
	resetRegistry(t)
	tmp := t.TempDir()
	opts, err := LoadExtensionOptions(filepath.Join(tmp, "absent.toml"))
	if err != nil {
		t.Errorf("missing file should not error, got: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("missing file should produce no options, got %d", len(opts))
	}
}

// TestExtensionRegistry_CustomExtensionWorksEndToEnd: any
// package — including operator-side custom extensions — can
// register a decoder via RegisterExtensionDecoder. This is
// the contract that lets users ship their own extensions
// with TOML configuration without needing to vendor or fork
// nexus.
//
// The "init() registers" pattern lives in the operator's own
// extension package; main.go imports the package (with or
// without alias), the init() runs once at process start, the
// decoder appears in the registry, the [extensions.<name>]
// block in nexus.toml resolves to its Options.
func TestExtensionRegistry_CustomExtensionWorksEndToEnd(t *testing.T) {
	resetRegistry(t)

	// Simulate an operator-side custom extension. Their
	// init() would do this; we call it directly here.
	type myCustomConfig struct {
		ServerURL string
		APIKey    string
	}
	var captured myCustomConfig
	RegisterExtensionDecoder("my-custom-ext", func(raw []byte) ([]Option, error) {
		// In real code, decoder would toml.Unmarshal raw
		// into a typed struct. For the test we just verify
		// the bytes reached our decoder.
		captured.ServerURL = extractField(string(raw), "server_url")
		captured.APIKey = extractField(string(raw), "api_key")
		return []Option{stubOption("custom")}, nil
	})

	// Operator's nexus.toml declares the extension.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	if err := os.WriteFile(path, []byte(`
[extensions.my-custom-ext]
server_url = "https://api.mycompany.internal"
api_key = "secret-key-from-env"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, err := LoadExtensionOptions(path)
	if err != nil {
		t.Fatalf("LoadExtensionOptions: %v", err)
	}
	if len(opts) != 1 {
		t.Errorf("expected 1 option from custom ext, got %d", len(opts))
	}
	if !strings.Contains(captured.ServerURL, "api.mycompany.internal") {
		t.Errorf("server_url not captured: %q", captured.ServerURL)
	}
}

// extractField is a tiny utility — finds `key = "value"` in
// a TOML fragment + returns the raw value text. Sufficient
// for the EndToEnd test; real decoders use toml.Unmarshal.
func extractField(src, key string) string {
	idx := strings.Index(src, key+" = ")
	if idx < 0 {
		return ""
	}
	start := idx + len(key) + 3
	end := strings.IndexByte(src[start:], '\n')
	if end < 0 {
		return strings.TrimSpace(src[start:])
	}
	return strings.TrimSpace(src[start : start+end])
}

// TestLoadExtensionOptions_NoExtensionsBlock: a nexus.toml
// without any [extensions.*] is also a clean no-op.
func TestLoadExtensionOptions_NoExtensionsBlock(t *testing.T) {
	resetRegistry(t)
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	if err := os.WriteFile(path, []byte(`
[runtime]
environment = "production"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, err := LoadExtensionOptions(path)
	if err != nil {
		t.Fatalf("LoadExtensionOptions: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("expected 0 options, got %d", len(opts))
	}
}

// resetRegistry empties the global extension registry between
// tests so they don't bleed into each other. Production code
// doesn't need this — registrations happen exactly once at
// process init.
func resetRegistry(t *testing.T) {
	t.Helper()
	extensionRegistry.Lock()
	extensionRegistry.m = map[string]ExtensionDecoder{}
	extensionRegistry.Unlock()
}

// noopDecoder is a no-op decoder for tests that only care
// about the registry's bookkeeping.
func noopDecoder(raw []byte) ([]Option, error) {
	return nil, nil
}

// stubOption returns a value implementing nexus.Option without
// actually doing anything. Sufficient for tests that count
// Options without applying them.
func stubOption(_ string) Option {
	return Raw(di.Options())
}
