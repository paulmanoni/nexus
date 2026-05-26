package nexus

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/pelletier/go-toml/v2"
)

// ExtensionDecoder turns the raw TOML bytes of one
// [extensions.<name>] sub-tree into the nexus.Option(s) that
// wire the extension into nexus.Run. The decoder owns its
// schema — operators write TOML keys the decoder defines,
// the decoder unmarshal()s them however it likes (typed
// struct, generic map, whatever).
//
// Returns the decoded Options + an error. Empty/nil Options
// + nil error means "successfully decoded but the block has
// no effect" (e.g. all fields zero); the framework treats it
// as a no-op.
//
// Why bytes rather than a typed struct: we don't know the
// extension's schema at framework-level, so we leave that
// decision to the decoder. Re-marshaling adds ~µs per
// extension at boot; negligible compared to the rest of
// startup cost.
type ExtensionDecoder func(rawTOML []byte) ([]Option, error)

// extensionRegistry holds the (name → decoder) map. Populated
// by extension package init() functions OR by explicit
// RegisterExtensionDecoder calls in tests / main.go.
//
// Mutex-protected because init() runs are technically
// sequential per package but operators MAY call register from
// dynamically-loaded code paths (plugins, test setup helpers).
var extensionRegistry = struct {
	sync.RWMutex
	m map[string]ExtensionDecoder
}{m: map[string]ExtensionDecoder{}}

// RegisterExtensionDecoder makes name a recognized
// [extensions.<name>] block in nexus.toml. The decoder gets
// the raw TOML bytes of just that sub-tree.
//
// Idiomatic usage: each extension's init() registers itself:
//
//	package myext
//
//	func init() {
//	    nexus.RegisterExtensionDecoder("myext", decode)
//	}
//
//	func decode(raw []byte) ([]nexus.Option, error) {
//	    var c MyExtConfig
//	    if err := toml.Unmarshal(raw, &c); err != nil { return nil, err }
//	    return []nexus.Option{Plugin(c)}, nil
//	}
//
// Calling RegisterExtensionDecoder twice with the same name
// replaces the prior decoder — operators with conflicting
// extension imports get the LAST registration, which is
// usually what they want when testing with a stub.
//
// Names are case-sensitive. Convention is lowercase kebab
// (`"myext"`, `"oauth2"`, `"rate-limit"`) matching the TOML
// idiom; the framework doesn't enforce this.
func RegisterExtensionDecoder(name string, dec ExtensionDecoder) {
	if name == "" || dec == nil {
		return
	}
	extensionRegistry.Lock()
	extensionRegistry.m[name] = dec
	extensionRegistry.Unlock()
}

// LookupExtensionDecoder returns the registered decoder for
// name, or nil when none is registered. Exposed for the lint
// command (so it can warn on [extensions.X] blocks for which
// no decoder exists — typically because the operator forgot
// to import the extension package).
func LookupExtensionDecoder(name string) ExtensionDecoder {
	extensionRegistry.RLock()
	defer extensionRegistry.RUnlock()
	return extensionRegistry.m[name]
}

// RegisteredExtensionNames returns the sorted list of every
// extension name a decoder is registered for. Useful for
// lint + diagnostic output ("declared: [a, b]; available:
// [a, b, c]").
func RegisteredExtensionNames() []string {
	extensionRegistry.RLock()
	defer extensionRegistry.RUnlock()
	names := make([]string, 0, len(extensionRegistry.m))
	for n := range extensionRegistry.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadExtensionOptions reads the [extensions.*] block from
// nexus.toml at path (defaults to DefaultConfigPath, same as
// LoadConfig), looks up each declared extension's decoder,
// and returns the collected Options ready to be spread into
// nexus.Run.
//
// Operators typically combine with LoadConfig:
//
//	cfg := nexus.MustLoadConfig()
//	extOpts, err := nexus.LoadExtensionOptions()
//	if err != nil { log.Fatal(err) }
//	opts := append(extOpts, /* hand-coded options */...)
//	nexus.Run(cfg, opts...)
//
// Or via the convenience helper LoadExtensions which panics:
//
//	nexus.Run(nexus.MustLoadConfig(), nexus.MustLoadExtensions()...)
//
// Behaviour:
//
//   - Missing file → returns ([], nil). Operators without a
//     nexus.toml pay nothing; existing code keeps working.
//   - [extensions.X] declared but no decoder registered for
//     X → returns a wrapped error citing X. Catch via lint at
//     CI time so the error never reaches boot.
//   - Decoder errors → wrapped with the extension name so the
//     operator knows which block was malformed.
//
// Ordering: decoders execute in alphabetical order of name
// for repeatable boot behavior. If you need a specific
// ordering (extension A depends on B's option) wire those
// via Go code instead — TOML is for data, not graph
// dependencies.
func LoadExtensionOptions(path ...string) ([]Option, error) {
	p := DefaultConfigPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	raw, err := readFileIfExists(p)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return decodeExtensions(raw)
}

// MustLoadExtensions is the panic-on-error variant matching
// MustLoadConfig's idiom. Use in main() when an extension
// block is required to boot.
func MustLoadExtensions(path ...string) []Option {
	opts, err := LoadExtensionOptions(path...)
	if err != nil {
		panic(err)
	}
	return opts
}

// decodeExtensions parses the [extensions.*] table out of
// rawTOML and dispatches each sub-table to the registered
// decoder. Pure function; factored out so unit tests can drive
// it with in-memory bytes.
func decodeExtensions(rawTOML []byte) ([]Option, error) {
	var doc extensionsDoc
	if err := toml.Unmarshal(rawTOML, &doc); err != nil {
		return nil, fmt.Errorf("nexus: parse extensions block: %w", err)
	}
	if len(doc.Extensions) == 0 {
		return nil, nil
	}
	// Alphabetical order for repeatable boot.
	names := make([]string, 0, len(doc.Extensions))
	for n := range doc.Extensions {
		names = append(names, n)
	}
	sort.Strings(names)

	var opts []Option
	for _, name := range names {
		sub := doc.Extensions[name]
		dec := LookupExtensionDecoder(name)
		if dec == nil {
			return nil, fmt.Errorf("nexus: no decoder registered for [extensions.%s] — did you forget to import the extension package?", name)
		}
		// Re-marshal the sub-tree so the decoder can
		// toml.Unmarshal it into its own typed struct.
		// Negligible cost; extension blocks are tiny.
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(sub); err != nil {
			return nil, fmt.Errorf("nexus: re-encode [extensions.%s]: %w", name, err)
		}
		got, err := dec(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("nexus: decode [extensions.%s]: %w", name, err)
		}
		opts = append(opts, got...)
	}
	return opts, nil
}

// extensionsDoc is the minimal TOML shape for parsing just
// the [extensions.*] table without claiming the rest of
// nexus.toml. Sibling tables ([runtime], [environments], etc.)
// get parsed by their own loaders.
type extensionsDoc struct {
	Extensions map[string]map[string]any `toml:"extensions"`
}

// readFileIfExists returns the file's bytes, or (nil, nil)
// when the file is absent. Other I/O errors propagate.
// Centralized so LoadExtensionOptions + lint use the same
// soft-miss policy.
func readFileIfExists(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}
