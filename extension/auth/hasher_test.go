package auth

import (
	"strings"
	"testing"
)

func TestHashersRoundTrip(t *testing.T) {
	t.Parallel()
	for _, h := range []Hasher{BCrypt(), Argon2id(), PBKDF2()} {
		t.Run(h.ID(), func(t *testing.T) {
			t.Parallel()
			encoded, err := h.Hash("correct horse battery staple")
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			if !strings.HasPrefix(encoded, h.ID()+"$") {
				t.Fatalf("encoded %q lacks %q prefix", encoded, h.ID())
			}
			ok, _, err := h.Verify("correct horse battery staple", encoded)
			if err != nil || !ok {
				t.Fatalf("Verify correct: ok=%v err=%v", ok, err)
			}
			bad, _, err := h.Verify("wrong password", encoded)
			if err != nil || bad {
				t.Fatalf("Verify wrong: ok=%v err=%v", bad, err)
			}
		})
	}
}

func TestHashSaltsDiffer(t *testing.T) {
	t.Parallel()
	h := Argon2id()
	a, _ := h.Hash("same")
	b, _ := h.Hash("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical — salt not applied")
	}
}

// TestHashersVerifyAcrossAlgorithms proves a Hashers set verifies a hash
// written by a NON-default algorithm and flags it for upgrade — the
// transparent-migration behavior.
func TestHashersVerifyAcrossAlgorithms(t *testing.T) {
	t.Parallel()
	set := DefaultHashers() // default bcrypt, all = [bcrypt, argon2id, pbkdf2]

	// Store a password hashed with argon2 (a non-default algorithm).
	legacy, err := Argon2id().Hash("s3cret-pw")
	if err != nil {
		t.Fatal(err)
	}
	ok, needsUpgrade, err := set.Verify("s3cret-pw", legacy)
	if err != nil || !ok {
		t.Fatalf("verify legacy: ok=%v err=%v", ok, err)
	}
	if !needsUpgrade {
		t.Fatal("a non-default algorithm should be flagged needsUpgrade")
	}

	// A hash by the default algorithm at current params needs no upgrade.
	current, _ := set.Hash("s3cret-pw")
	ok, needsUpgrade, err = set.Verify("s3cret-pw", current)
	if err != nil || !ok || needsUpgrade {
		t.Fatalf("verify current: ok=%v upgrade=%v err=%v", ok, needsUpgrade, err)
	}
}

func TestBCryptCostUpgrade(t *testing.T) {
	t.Parallel()
	// A hash at a lower cost than the verifier's default should upgrade.
	low, _ := BCryptCost(10).Hash("pw12345")
	ok, upgrade, err := BCryptCost(12).Verify("pw12345", low)
	if err != nil || !ok {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
	if !upgrade {
		t.Fatal("lower-cost bcrypt hash should be flagged for upgrade")
	}
}

func TestHashersUnknownPrefix(t *testing.T) {
	t.Parallel()
	if _, _, err := DefaultHashers().Verify("x", "sha1$deadbeef"); err != ErrUnknownHash {
		t.Fatalf("want ErrUnknownHash, got %v", err)
	}
}
