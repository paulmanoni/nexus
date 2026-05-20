package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSealUnseal_RoundTrip drives the headline contract: a
// plaintext blob sealed with a given key, then unsealed with the
// same key, returns the original byte-for-byte. Without this,
// every higher-level test is built on sand.
func TestSealUnseal_RoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("hello sealed world\nyaml: { with: nested: values }\n")

	sealed, err := Seal(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) {
		t.Error("IsSealed = false on freshly-sealed blob")
	}
	if bytes.Equal(plaintext, sealed) {
		t.Error("sealed blob equals plaintext — no encryption happened")
	}
	unsealed, err := Unseal(sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, unsealed) {
		t.Errorf("unsealed != plaintext\n  want: %q\n   got: %q", plaintext, unsealed)
	}
}

// TestUnseal_WrongKey proves AES-GCM's authentication catches a
// bad-key attempt. Without this, an attacker who got the
// ciphertext but not the key could still get garbage that LOOKS
// like yaml, and the parser would surface the failure way later
// in the boot chain instead of at the decrypt site.
func TestUnseal_WrongKey(t *testing.T) {
	keyA, _ := GenerateKey()
	keyB, _ := GenerateKey()
	sealed, _ := Seal([]byte("secret"), keyA)
	_, err := Unseal(sealed, keyB)
	if err == nil {
		t.Fatal("Unseal with wrong key should error")
	}
}

// TestUnseal_TamperedFile proves the GCM auth tag catches a
// single-byte flip in the ciphertext. Verifies BEFORE returning
// any plaintext — Unseal must NOT return partial output even
// when only the tag fails.
func TestUnseal_TamperedFile(t *testing.T) {
	key, _ := GenerateKey()
	sealed, _ := Seal([]byte("secret yaml body"), key)

	// Flip a byte deep in the body (past header + nonce).
	sealed[len(sealed)-5] ^= 0xFF
	_, err := Unseal(sealed, key)
	if err == nil {
		t.Fatal("Unseal of tampered file should error")
	}
}

// TestLoadSealKey_RefusesPermissive proves the perm check fires.
// A world-readable seal key defeats the whole point; we MUST
// surface this at boot.
func TestLoadSealKey_RefusesPermissive(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "seal.key")
	key, _ := GenerateKey()
	if err := os.WriteFile(keyPath, key, 0o644); err != nil { // too permissive
		t.Fatal(err)
	}
	_, err := LoadSealKey(keyPath)
	if err == nil {
		t.Fatal("LoadSealKey should refuse 0o644 perms")
	}
}

// TestLoadSealKey_AcceptsRawAndBase64 proves both common storage
// shapes work — raw 32-byte file (CLI keygen default) and base64
// (for environments that need ASCII-safe transit).
func TestLoadSealKey_AcceptsRawAndBase64(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, keySize)
	for i := range key {
		key[i] = byte(i)
	}

	rawPath := filepath.Join(dir, "raw.key")
	if err := os.WriteFile(rawPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSealKey(rawPath)
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Error("raw round-trip mismatch")
	}

	// Same key, base64 form.
	b64Path := filepath.Join(dir, "b64.key")
	b64 := []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=") // base64(0..31)
	if err := os.WriteFile(b64Path, b64, 0o600); err != nil {
		t.Fatal(err)
	}
	got2, err := LoadSealKey(b64Path)
	if err != nil {
		t.Fatalf("b64: %v", err)
	}
	if !bytes.Equal(got2, key) {
		t.Errorf("b64 round-trip mismatch:\n  want: %v\n   got: %v", key, got2)
	}
}

// TestMaybeUnseal_PassesPlaintextThrough proves the auto-detect
// path: a non-sealed blob is returned verbatim. Required so
// config.Local can handle both first-boot (plaintext) and
// repeated-boot (sealed) inputs without branching at the call
// site.
func TestMaybeUnseal_PassesPlaintextThrough(t *testing.T) {
	plain := []byte("plain yaml: contents")
	out, err := MaybeUnseal(plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plain) {
		t.Errorf("plaintext not passed through verbatim")
	}
}
