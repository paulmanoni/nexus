package main

// Blank-import every framework extension that ships an
// [extensions.<name>] TOML decoder, so its init() runs inside
// the CLI binary. Each such init() calls
// nexus.RegisterExtensionDecoder, which is what lets `nexus
// lint` validate an [extensions.<name>] block against the real
// decoder instead of warning that it doesn't recognize the name.
//
// Today that is exactly ONE package: extension/config. Verify
// before adding to this list:
//
//	grep -rn 'RegisterExtensionDecoder(' --include='*.go' .
//
// Every OTHER extension is wired in Go, not through
// [extensions.*] — auth/inertia/maskid/session/… take a Module
// or Bind option, and cache/mail/storage read their own
// TOP-LEVEL tables ([cache.*], [mail.*], [storage.*]) via
// BindFromConfig. So a block like [extensions.storage] is not a
// real nexus.toml surface, and the lint warning it produces is
// correct, not a false positive: nexus.LoadExtensionOptions
// fails the app's own boot on the same name. Blank-importing
// those packages here would register nothing, pull their
// dependency trees into the CLI, and silence nothing.
//
// Custom extensions written by operators can't be visible here
// either — those live in the operator's own project and only
// register in the app binary. That's why the lint emits a
// WARNING rather than an error: a custom extension must not
// false-fail CI.
//
// Listed alphabetically.
import (
	_ "github.com/paulmanoni/nexus/extension/config"
)
