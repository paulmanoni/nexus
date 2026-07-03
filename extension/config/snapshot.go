package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/paulmanoni/nexus/extension/config/internal/canonical"
)

// Snapshot is the on-the-wire shape served at GET /__config/snapshot/:app/:profile.
// Carries the resolved value tree plus enough metadata for the client to
// verify it and decide whether anything changed since the last fetch.
type Snapshot struct {
	App      string         `json:"app"`
	Profile  string         `json:"profile"`
	Label    string         `json:"label,omitempty"` // git ref; empty for local sources
	Version  string         `json:"version"`         // content-addressed: hash of values
	ServedAt time.Time      `json:"served_at"`
	Values   map[string]any `json:"values"` // the merged value tree
}

// SignedSnapshot is the actual wire envelope. The signature is over
// canonical-JSON(Snapshot) — clients reproduce the canonical
// serialization locally and verify against the public key pinned
// in their config (SignerKey).
//
// KID lets the server rotate signing keys: clients pin N pubkeys
// (current + retiring); the snapshot's KID picks which one to
// verify against.
type SignedSnapshot struct {
	Snapshot  Snapshot `json:"snapshot"`
	KID       string   `json:"kid"`
	Signature string   `json:"signature"` // base64 raw-std encoded ed25519 sig
}

// Sign produces a SignedSnapshot. The signing key MUST be the
// Ed25519 private key whose public counterpart is pinned by every
// client that will accept this snapshot — kid is the human label
// the client uses to pick which pinned pubkey to verify against.
func Sign(s Snapshot, priv ed25519.PrivateKey, kid string) (SignedSnapshot, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return SignedSnapshot{}, fmt.Errorf("config: signing key wrong size: got %d, want %d",
			len(priv), ed25519.PrivateKeySize)
	}
	// Canonicalize the Snapshot for stable bytes. We round-trip
	// through encoding/json once to get a map[string]any (the
	// shape the canonical emitter understands natively) so
	// struct-field ordering doesn't leak into the signing input.
	raw, err := json.Marshal(s)
	if err != nil {
		return SignedSnapshot{}, fmt.Errorf("config: marshal snapshot: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return SignedSnapshot{}, fmt.Errorf("config: re-decode snapshot: %w", err)
	}
	body, err := canonical.Marshal(generic)
	if err != nil {
		return SignedSnapshot{}, fmt.Errorf("config: canonicalize snapshot: %w", err)
	}
	sig := ed25519.Sign(priv, body)
	return SignedSnapshot{
		Snapshot:  s,
		KID:       kid,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// Verify checks ss against one or more pinned public keys, keyed
// by KID. Returns nil iff the signature is valid AND the snapshot's
// KID matches one of the pinned keys.
//
// Multiple pubkeys in the map cover key-rotation: during a
// rotation window the server can sign with KID "v2" while clients
// accept both "v1" and "v2" until the next deploy retires "v1".
// An unknown KID is an error, not a soft-fail — refusing to
// accept a key the operator didn't pin is the whole point of
// signature verification.
func Verify(ss SignedSnapshot, pinned map[string]ed25519.PublicKey) error {
	pub, ok := pinned[ss.KID]
	if !ok {
		return fmt.Errorf("config: snapshot signed with KID %q which is not in the pinned key set", ss.KID)
	}
	sig, err := base64.StdEncoding.DecodeString(ss.Signature)
	if err != nil {
		return fmt.Errorf("config: decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("config: signature wrong size: got %d, want %d", len(sig), ed25519.SignatureSize)
	}
	// Reproduce the canonical signing input.
	raw, err := json.Marshal(ss.Snapshot)
	if err != nil {
		return fmt.Errorf("config: marshal snapshot for verify: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("config: re-decode snapshot for verify: %w", err)
	}
	body, err := canonical.Marshal(generic)
	if err != nil {
		return fmt.Errorf("config: canonicalize snapshot for verify: %w", err)
	}
	if !ed25519.Verify(pub, body, sig) {
		return errors.New("config: signature does not match snapshot under pinned key")
	}
	return nil
}

// Resolve walks dotted-path key in the snapshot's value tree.
// "config.api.timeout" → values["config"]["api"]["timeout"].
// Returns (value, true) when the full path resolves; (nil, false)
// at the first missing key. Stops at scalars — a path that
// descends into a string is a path that doesn't exist.
func (s Snapshot) Resolve(key string) (any, bool) {
	if key == "" {
		return s.Values, true
	}
	var cur any = s.Values
	parts := splitDotted(key)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// splitDotted is faster than strings.Split for the path-walking
// hot path because we don't allocate the intermediate slice the
// stdlib version always builds.
func splitDotted(key string) []string {
	n := 1
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			n++
		}
	}
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			out = append(out, key[start:i])
			start = i + 1
		}
	}
	out = append(out, key[start:])
	return out
}
