package oauth2

// SHA1 is intentional and legacy-interop only: VerifySpringPassword
// has to read passwords already on disk that were hashed with
// Spring's StandardPasswordEncoder (pre-5.0). The framework never
// produces new SHA1 hashes; only verifies existing ones for apps
// migrating off Spring.
//
// #nosec G505 -- legacy Spring StandardPasswordEncoder interop
import (
	"crypto/sha1" // #nosec G505 -- legacy interop, verify-only
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// VerifyBcrypt compares input against a bcrypt hash. Accepts both
// raw bcrypt strings and {bcrypt}-prefixed values; returns false
// for any other format. Constant-time semantics from x/crypto.
func VerifyBcrypt(stored, input string) bool {
	stored = strings.TrimPrefix(stored, "{bcrypt}")
	if stored == "" {
		return false
	}
	if !(strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") || strings.HasPrefix(stored, "$2y$")) {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(stored), []byte(input)) == nil
}

// VerifySpringPassword tries every password format Spring Security's
// DelegatingPasswordEncoder emits in the wild, in priority order:
//
//   - {bcrypt}$2a$… — modern Spring default
//   - {noop}plain   — disabled-encoding marker (test fixtures)
//   - $2a$ / $2b$ / $2y$ — raw bcrypt without the {scheme} prefix
//   - 40-char hex with a 10-char salt prefix — legacy salted-sha1
//     (Spring's StandardPasswordEncoder pre-5.0)
//
// Returns (ok, scheme) — scheme is the matched bucket or "" on
// miss, useful for logging which path resolved the credential.
// Never logs the input.
func VerifySpringPassword(stored, input string) (ok bool, scheme string) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return false, ""
	}

	if strings.HasPrefix(stored, "{") {
		end := strings.Index(stored, "}")
		if end < 0 {
			return false, "malformed"
		}
		s := strings.ToLower(stored[1:end])
		rest := stored[end+1:]
		switch s {
		case "noop":
			return rest == input, "noop"
		case "bcrypt":
			return bcrypt.CompareHashAndPassword([]byte(rest), []byte(input)) == nil, "bcrypt"
		default:
			return false, s
		}
	}

	if strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") || strings.HasPrefix(stored, "$2y$") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(input)) == nil, "bcrypt"
	}

	if len(stored) == 40 && isLowerHex(stored[10:]) {
		salt := stored[:10]
		sum := sha1.Sum([]byte(salt + input)) // #nosec G401 -- legacy Spring interop
		expected := salt + hex.EncodeToString(sum[:])[:30]
		return expected == stored, "salted-sha1"
	}

	return false, ""
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if !(('0' <= r && r <= '9') || ('a' <= r && r <= 'f')) {
			return false
		}
	}
	return true
}
