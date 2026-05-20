package config

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Sealed-file format. Four bytes of magic + one version byte + a
// 12-byte AES-GCM nonce + ciphertext (which includes the 16-byte
// GCM auth tag). Auto-detected on read so config.Local accepts
// both sealed and plaintext yaml — the file extension is
// convention, not load-bearing.
//
//	magic    "NXCS" (4 bytes)
//	version  0x01    (1 byte; future format changes)
//	nonce    12 random bytes
//	body     AES-256-GCM(plaintext, key, nonce)  (ct + 16-byte tag)
const (
	sealMagic   = "NXCS"
	sealVersion = byte(0x01)
	nonceSize   = 12 // standard for AES-GCM
	keySize     = 32 // AES-256
	headerSize  = len(sealMagic) + 1 + nonceSize
)

// IsSealed reports whether b looks like a sealed file. Cheap
// magic-byte check; the actual cryptographic verification fires
// inside Unseal via AES-GCM's auth tag.
func IsSealed(b []byte) bool {
	return len(b) >= headerSize &&
		bytes.HasPrefix(b, []byte(sealMagic)) &&
		b[len(sealMagic)] == sealVersion
}

// GenerateKey returns 32 cryptographically-random bytes suitable
// for AES-256. The framework's `nexus config keygen` CLI writes
// this to a 0o600 file the operator pins via SealKey.
func GenerateKey() ([]byte, error) {
	k := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, fmt.Errorf("config: keygen: %w", err)
	}
	return k, nil
}

// Seal encrypts plaintext under key and returns a self-contained
// blob suitable for writing to disk verbatim. Nonce is fresh per
// call — DO NOT reuse output across versions of the same logical
// file without re-sealing (the nonce changes every time, which is
// the point; reuse breaks GCM's security argument).
func Seal(plaintext, key []byte) ([]byte, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("config: seal key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("config: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("config: cipher.NewGCM: %w", err)
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("config: nonce: %w", err)
	}
	// Header is also AAD — a tampered version byte fails auth even
	// if the attacker has the key.
	header := make([]byte, 0, headerSize)
	header = append(header, sealMagic...)
	header = append(header, sealVersion)
	header = append(header, nonce...)

	ct := gcm.Seal(nil, nonce, plaintext, header)

	out := make([]byte, 0, len(header)+len(ct))
	out = append(out, header...)
	out = append(out, ct...)
	return out, nil
}

// Unseal reverses Seal. The GCM auth tag is verified before any
// plaintext is returned; a tampered file produces an error and
// not partial output. Mismatched key produces "auth failure"
// rather than gibberish.
func Unseal(sealed, key []byte) ([]byte, error) {
	if len(sealed) < headerSize {
		return nil, errors.New("config: sealed blob too short")
	}
	if !IsSealed(sealed) {
		return nil, errors.New("config: sealed blob missing NXCS magic")
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("config: seal key must be %d bytes, got %d", keySize, len(key))
	}
	header := sealed[:headerSize]
	nonce := sealed[len(sealMagic)+1 : headerSize]
	ct := sealed[headerSize:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("config: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("config: cipher.NewGCM: %w", err)
	}
	pt, err := gcm.Open(nil, nonce, ct, header)
	if err != nil {
		return nil, fmt.Errorf("config: unseal: auth failed (wrong key or tampered file): %w", err)
	}
	return pt, nil
}

// LoadSealKey reads a key file and returns the 32 raw bytes.
// Framework-internal: the public Local/Client entrypoints never
// expose a key path; the framework picks a sibling .key file
// next to the sealed payload, generates it at first boot, and
// reads it via this helper on subsequent boots. CLI subcommands
// (nexus config unseal) call it for incident-response use.
//
// Accepts both raw (32-byte file) and base64-encoded (44-character
// file with optional trailing newline) formats. Refuses to read
// a file whose mode is more permissive than 0o640 — a world-
// readable seal key defeats the point.
func LoadSealKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config: stat seal key %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("config: seal key %s perm %#o too permissive (require <=0640)", path, mode)
	}
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("config: read seal key %s: %w", path, err)
	}
	// Trim trailing whitespace + newline so an editor-saved file
	// works without surgery.
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == keySize {
		return trimmed, nil // raw bytes
	}
	// Try base64 decode.
	if dec, err := base64.StdEncoding.DecodeString(string(trimmed)); err == nil && len(dec) == keySize {
		return dec, nil
	}
	// Last-chance: maybe the file has a trailing literal newline
	// inside the raw bytes (rare but possible if generated with
	// a buggy `head -c 32` pipeline). Accept exactly-32 with no
	// trimming as a fallback.
	if len(body) == keySize {
		return body, nil
	}
	return nil, fmt.Errorf("config: seal key %s: expected 32 raw bytes or base64; got %d bytes",
		path, len(body))
}

// MaybeUnseal is the load-side convenience: if data is sealed,
// unseal with key; otherwise return data verbatim. Used by
// config.Local and the client cache reader so callers don't
// have to branch on file shape.
func MaybeUnseal(data, key []byte) ([]byte, error) {
	if !IsSealed(data) {
		return data, nil
	}
	if len(key) == 0 {
		return nil, errors.New("config: file is sealed but no SealKey configured")
	}
	return Unseal(data, key)
}

// ext is the conventional suffix the CLI emits + the Local
// constructor's auto-detection hint for "this is a sealed file."
// Magic-byte detection is authoritative; the extension is a
// human-readable hint for operators.
const sealedFileExt = ".sealed"

// IsLikelySealedPath reports whether p ends with the sealed
// extension. Pure convention — operators can name sealed files
// anything they want; magic-byte detection still works.
func IsLikelySealedPath(p string) bool {
	return strings.HasSuffix(p, sealedFileExt)
}
