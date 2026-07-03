package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// Hasher is one password-hashing algorithm — the Django PASSWORD_HASHERS
// analogue. An app plugs in a Hashers set; the default hashes new
// passwords, and every hasher in the set can verify a stored hash so you
// can migrate algorithms transparently (rehash-on-login).
//
// Encoded strings are self-describing: "<id>$<payload>", so a Hashers set
// dispatches Verify to the right algorithm by the prefix before the first
// "$" — the same scheme Django uses.
type Hasher interface {
	// ID is the algorithm label and the encoded-string prefix
	// ("bcrypt", "argon2id", "pbkdf2_sha256").
	ID() string

	// Hash returns an encoded "<id>$<payload>" string for password.
	Hash(password string) (string, error)

	// Verify checks password against an encoded string produced by THIS
	// hasher. needsUpgrade is true when the stored parameters are weaker
	// than this hasher's current settings (e.g. a lower bcrypt cost), so
	// the caller can rehash on a successful login.
	Verify(password, encoded string) (ok bool, needsUpgrade bool, err error)
}

// ErrUnknownHash is returned by a Hashers set when no member recognizes an
// encoded string's algorithm prefix.
var ErrUnknownHash = errors.New("auth: no hasher recognizes this encoded password")

// Hashers is an ordered set of password hashers. Default hashes new
// passwords; All is consulted (by algorithm prefix) to verify existing
// ones, so legacy hashes keep working while new writes use Default.
type Hashers struct {
	Default Hasher
	All     []Hasher
}

// DefaultHashers returns the recommended set: bcrypt for new passwords
// (predictable memory, no tuning traps), with argon2id and pbkdf2_sha256
// included so hashes written by those algorithms still verify — and get
// transparently rehashed to bcrypt on the next login.
func DefaultHashers() Hashers {
	bc := BCrypt()
	return Hashers{Default: bc, All: []Hasher{bc, Argon2id(), PBKDF2()}}
}

// Hash encodes password with the default hasher.
func (h Hashers) Hash(password string) (string, error) {
	if h.Default == nil {
		return "", errors.New("auth: Hashers.Default is nil")
	}
	return h.Default.Hash(password)
}

// Verify finds the hasher matching encoded's prefix and checks password.
// needsUpgrade is true when the matching hasher reports weaker-than-current
// parameters OR the hash was written by a non-default algorithm — either
// way the caller should rehash with Default on success.
func (h Hashers) Verify(password, encoded string) (ok bool, needsUpgrade bool, err error) {
	id, _, found := strings.Cut(encoded, "$")
	if !found {
		return false, false, ErrUnknownHash
	}
	for _, hasher := range h.All {
		if hasher.ID() != id {
			continue
		}
		ok, up, err := hasher.Verify(password, encoded)
		if err != nil {
			return false, false, err
		}
		if h.Default != nil && hasher.ID() != h.Default.ID() {
			up = true // not the default algorithm → migrate on login
		}
		return ok, up, nil
	}
	return false, false, ErrUnknownHash
}

// randSalt returns n cryptographically random bytes, panicking only if the
// system CSPRNG is unavailable (an unrecoverable condition for hashing).
func randSalt(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return b
}

// --- bcrypt -----------------------------------------------------------------

// bcryptHasher wraps golang.org/x/crypto/bcrypt. Its encoded form is
// "bcrypt$<native-bcrypt-string>" so it fits the "<id>$<payload>" scheme.
type bcryptHasher struct{ cost int }

// BCrypt returns a bcrypt hasher at cost 12 (a sensible 2020s default).
func BCrypt() Hasher { return bcryptHasher{cost: 12} }

// BCryptCost returns a bcrypt hasher at an explicit cost (4–31).
func BCryptCost(cost int) Hasher { return bcryptHasher{cost: cost} }

func (bcryptHasher) ID() string { return "bcrypt" }

func (b bcryptHasher) Hash(password string) (string, error) {
	out, err := bcrypt.GenerateFromPassword([]byte(password), b.cost)
	if err != nil {
		return "", err
	}
	return "bcrypt$" + string(out), nil
}

func (b bcryptHasher) Verify(password, encoded string) (bool, bool, error) {
	native := strings.TrimPrefix(encoded, "bcrypt$")
	if err := bcrypt.CompareHashAndPassword([]byte(native), []byte(password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, false, nil
		}
		return false, false, err
	}
	cost, err := bcrypt.Cost([]byte(native))
	if err != nil {
		return true, false, nil
	}
	return true, cost < b.cost, nil
}

// --- argon2id ---------------------------------------------------------------

// argon2Params are the argon2id cost parameters. Defaults balance strength
// and per-hash memory (64 MiB) so a login burst can't exhaust a small
// container.
type argon2Hasher struct {
	time    uint32
	memory  uint32 // KiB
	threads uint8
	keyLen  uint32
	saltLen int
}

// Argon2id returns an argon2id hasher with balanced defaults
// (t=2, m=64MiB, p=2).
func Argon2id() Hasher {
	return argon2Hasher{time: 2, memory: 64 * 1024, threads: 2, keyLen: 32, saltLen: 16}
}

func (argon2Hasher) ID() string { return "argon2id" }

func (a argon2Hasher) Hash(password string) (string, error) {
	salt := randSalt(a.saltLen)
	key := argon2.IDKey([]byte(password), salt, a.time, a.memory, a.threads, a.keyLen)
	// argon2id$v=19$m=<mem>,t=<time>,p=<threads>$<salt>$<hash>
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, a.memory, a.time, a.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func (a argon2Hasher) Verify(password, encoded string) (bool, bool, error) {
	parts := strings.Split(encoded, "$")
	// ["argon2id","v=19","m=..,t=..,p=..","<salt>","<hash>"]
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false, false, fmt.Errorf("auth: malformed argon2 hash")
	}
	var mem, tm uint32
	var par uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &mem, &tm, &par); err != nil {
		return false, false, fmt.Errorf("auth: malformed argon2 params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, tm, mem, par, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	upgrade := mem < a.memory || tm < a.time || par < a.threads
	return true, upgrade, nil
}

// --- pbkdf2 -----------------------------------------------------------------

type pbkdf2Hasher struct {
	iterations int
	keyLen     int
	saltLen    int
}

// PBKDF2 returns a PBKDF2-HMAC-SHA256 hasher (600k iterations), matching
// the modern Django default — useful for interop with a Django database.
func PBKDF2() Hasher {
	return pbkdf2Hasher{iterations: 600_000, keyLen: 32, saltLen: 16}
}

func (pbkdf2Hasher) ID() string { return "pbkdf2_sha256" }

func (p pbkdf2Hasher) Hash(password string) (string, error) {
	salt := randSalt(p.saltLen)
	key, err := pbkdf2.Key(sha256.New, password, salt, p.iterations, p.keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", p.iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func (p pbkdf2Hasher) Verify(password, encoded string) (bool, bool, error) {
	parts := strings.Split(encoded, "$")
	// ["pbkdf2_sha256","<iter>","<salt>","<hash>"]
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false, false, fmt.Errorf("auth: malformed pbkdf2 hash")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, false, fmt.Errorf("auth: malformed pbkdf2 iterations: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, false, err
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, false, err
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, iter < p.iterations, nil
}
