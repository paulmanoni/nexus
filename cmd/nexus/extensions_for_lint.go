package main

// Blank-import every framework extension so its init() runs
// inside the CLI binary. Each init() registers its TOML
// decoder via nexus.RegisterExtensionDecoder, which lets
// `nexus lint` validate [extensions.<name>] blocks against
// the real decoders.
//
// Custom extensions written by operators won't be visible
// here — those live in the operator's own project and only
// register in the app binary. The lint emits a WARNING (not
// an error) for unknown extensions so a custom extension
// doesn't false-fail CI.
//
// Listed alphabetically. Add new framework extensions here
// when they ship their own decoder.
import (
	_ "github.com/paulmanoni/nexus/extension/config"
)
