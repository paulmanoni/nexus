package nexus

import "encoding/base64"

// embeddedConfigB64 holds a base64-encoded copy of nexus.toml baked into
// the binary at build time. `nexus build` sets it via the linker:
//
//	go build -ldflags "-X github.com/paulmanoni/nexus.embeddedConfigB64=<base64>"
//
// so the binary ships as a single self-contained artifact — no nexus.toml
// needs to travel alongside it. It stays empty for a plain `go build`,
// leaving the disk-based resolution (NEXUS_CONFIG → cwd → next to the
// executable) as the only source.
//
// The embedded copy is the RAW file, ${VAR} placeholders intact, so
// secrets are NOT baked in — they resolve from the runtime environment
// when Boot expands the TOML (see configFromTOML). base64 keeps the value
// a single line with no characters the linker's -X flag mishandles.
var embeddedConfigB64 string

// embeddedConfig returns the decoded embedded nexus.toml and whether one
// was baked in. A malformed value (should never happen — the build writes
// it) is treated as absent rather than fatal, so a corrupt stamp can't
// brick startup when a disk config is also present.
func embeddedConfig() ([]byte, bool) {
	if embeddedConfigB64 == "" {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(embeddedConfigB64)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}
