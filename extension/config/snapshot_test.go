package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// TestSign_VerifyRoundTrip is the headline cryptographic
// contract: a snapshot signed with priv verifies against the
// matching pub. Without this, every client would reject every
// snapshot and the whole feature falls apart.
func TestSign_VerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	snap := Snapshot{
		App:      "app1",
		Profile:  "prod",
		Version:  "abc123",
		ServedAt: time.Now().UTC(),
		Values: map[string]any{
			"api": map[string]any{"timeout": "5s"},
		},
	}
	signed, err := Sign(snap, priv, "test-kid")
	if err != nil {
		t.Fatal(err)
	}
	if signed.KID != "test-kid" {
		t.Errorf("KID = %q, want test-kid", signed.KID)
	}
	pinned := map[string]ed25519.PublicKey{"test-kid": pub}
	if err := Verify(signed, pinned); err != nil {
		t.Errorf("Verify on freshly-signed snapshot: %v", err)
	}
}

// TestVerify_RejectsWrongKID proves the KID gate fires. A
// snapshot claiming to be signed with a key the client didn't
// pin is rejected — operator's expected set of valid keys is
// the security boundary, not "any key the server happens to
// hold."
func TestVerify_RejectsWrongKID(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signed, _ := Sign(Snapshot{App: "x", Profile: "y", Values: map[string]any{}}, priv, "issued-kid")

	pinned := map[string]ed25519.PublicKey{"different-kid": pub}
	if err := Verify(signed, pinned); err == nil {
		t.Error("Verify should reject unknown KID")
	}
}

// TestVerify_RejectsTamperedValues proves the canonical-JSON
// signing input catches body tampering. An attacker who flipped
// even one byte in the values tree without forging the signature
// must be rejected.
func TestVerify_RejectsTamperedValues(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signed, _ := Sign(Snapshot{
		App:    "app",
		Values: map[string]any{"key": "original"},
	}, priv, "kid")

	signed.Snapshot.Values["key"] = "tampered"

	pinned := map[string]ed25519.PublicKey{"kid": pub}
	if err := Verify(signed, pinned); err == nil {
		t.Error("Verify should reject snapshot whose values were modified after signing")
	}
}

// TestSnapshot_ResolveDottedPath proves the dotted-path walker
// climbs nested maps cleanly. Same logic the root-package
// nexus.Get uses; tested here to keep the snapshot type's
// contract self-contained.
func TestSnapshot_ResolveDottedPath(t *testing.T) {
	s := Snapshot{Values: map[string]any{
		"config": map[string]any{
			"api": map[string]any{
				"timeout": "5s",
				"retries": 3,
			},
			"features": map[string]any{
				"new_checkout": true,
			},
		},
	}}
	cases := []struct {
		key    string
		want   any
		exists bool
	}{
		{"config.api.timeout", "5s", true},
		{"config.api.retries", 3, true},
		{"config.features.new_checkout", true, true},
		{"config.missing", nil, false},
		{"config.api.timeout.deeper", nil, false}, // descending past scalar
	}
	for _, c := range cases {
		got, ok := s.Resolve(c.key)
		if ok != c.exists {
			t.Errorf("%q: ok=%v want %v", c.key, ok, c.exists)
		}
		if c.exists && got != c.want {
			t.Errorf("%q: got %v, want %v", c.key, got, c.want)
		}
	}
}
