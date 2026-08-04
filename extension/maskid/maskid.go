// Package maskid replaces sequential integer IDs with opaque strings on
// the wire, without changing handler code.
//
// A REST response that used to read {"id": 41, "ownerId": 7} goes out as
// {"id": "9tKq3nB1wZ0aVdH7cRmXsA", "ownerId": "Lp2f…"}, and a request
// carrying those strings back — path segment, query value, JSON body,
// GraphQL argument — arrives at the handler as the integers 41 and 7
// again. Handlers, GORM models and SQL are untouched.
//
// Enable it with one option:
//
//	nexus.Boot(maskid.Module(maskid.Config{Key: os.Getenv("MASKID_KEY")}))
//
// # What this is and is not
//
// Masking removes *enumeration* and *inference*: a client can no longer
// count your users from an id, guess a neighbouring record, or correlate
// two resources by arithmetic. It is emphatically NOT access control. A
// masked id is still a bearer reference — anyone holding one can use it.
// Authorization checks on every query and mutation remain exactly as
// necessary as they were before.
//
// # The codec
//
// The default codec is a deterministic AES permutation over a single
// block: domain tag (8 bytes) ‖ big-endian id (8 bytes), encrypted and
// base64url-encoded to 22 characters. Deterministic means a given id
// always masks to the same string, so URLs stay bookmarkable and caches
// keep working. The domain tag is an integrity check — a forged or
// corrupted mask fails to decode rather than decoding into some other
// record's id.
//
// This is real encryption, not the reversible arithmetic of hashids or
// sqids: without the key an attacker cannot recover the underlying
// integer or forge a valid mask.
//
// # What gets masked
//
// By default, any JSON key named "id"/"ids" or ending in Id/ID/_id
// (singular or plural) whose value is a whole number: id, userId,
// owner_id, categoryIDs. Tune it with Include, Exclude or Match.
//
// # Scoping to part of an app
//
// Masking is app-wide by default. Config.Types (or MatchType) narrows it
// to named response types, which is what you want when some of your IDs
// also travel to a system outside this app — a legacy backend the same
// SPA calls, a partner webhook — and would arrive there as strings it
// can't use:
//
//	maskid.Module(maskid.Config{
//	    Key:   os.Getenv("MASKID_KEY"),
//	    Types: []string{"Invoice", "InvoiceLine", "Customer"},
//	})
//
// Only masking is scoped. Unmasking always runs, and needs no scope: a
// value is converted only when it decrypts, which happens only for a mask
// this app minted, so an out-of-scope type's plain integer passes through
// either way.
package maskid

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/internal/maskhook"
)

// KeyEnv is consulted when Config.Key is empty.
const KeyEnv = "NEXUS_MASKID_KEY"

// Config configures the masking extension. The zero value is usable in
// development (a random per-process key is generated, with a warning);
// production must set Key.
type Config struct {
	// Key is the secret the codec derives from. Any length — it is
	// hashed to 32 bytes. Losing or rotating it invalidates every mask
	// already handed out, so treat it like a session-signing key.
	//
	// Empty falls back to $NEXUS_MASKID_KEY, then to a random
	// per-process key (dev only — masks won't survive a restart).
	Key string

	// Include names fields to mask that the default policy would miss
	// (a "reference" or "code" column that is really a primary key).
	Include []string

	// Exclude names fields to leave alone. Applied after Include and
	// after Match, so it always wins — the escape hatch for an id that
	// a third party already knows, or one that must stay sortable.
	//
	// Excluding a key prunes its whole subtree, not just that scalar.
	// That is what makes reference data expressible: a lookup row's
	// primary key is spelled "id" like every other, so excluding "id"
	// is not an option — but excluding the field that HOLDS the lookups
	// ("categories", "regions") spares every ID inside it. Mask the
	// records, leave the code tables numeric.
	Exclude []string

	// Match replaces the built-in field policy entirely. Include and
	// Exclude still apply on top of it.
	Match func(key string) bool

	// Types scopes MASKING to these response types, named by their Go
	// type (which is also their GraphQL object name). Empty means the
	// whole app.
	//
	// Reach for this when masking isn't safe app-wide — most often when
	// some of your IDs also travel to a system outside this app (a
	// legacy backend the same SPA calls, a partner webhook) and would
	// arrive there as strings it can't use. Name the types that stay
	// inside; the rest keep their plain integers.
	//
	// Unmasking is never scoped, and doesn't need to be: a value is only
	// converted when it decrypts, which only happens for a mask this app
	// minted. An out-of-scope type's plain integer passes through
	// untouched either way, so a scope can never break an inbound
	// request.
	Types []string

	// MatchType is the predicate form of Types, for a scope too large or
	// too dynamic to list. Types and MatchType are OR-ed when both are
	// set.
	MatchType func(typeName string) bool

	// Codec replaces the built-in AES codec. Supply one to interoperate
	// with an existing scheme; Key is then ignored.
	Codec Codec
}

// Codec converts between an integer id and its wire form. Implementations
// must be deterministic (the same id always produces the same string)
// and must reject values they did not produce.
type Codec interface {
	Mask(id int64) string
	Unmask(s string) (int64, bool)
}

var (
	mu      sync.RWMutex
	current *policy
)

type policy struct {
	codec     Codec
	match     func(string) bool
	incl      map[string]bool
	excl      map[string]bool
	types     map[string]bool
	matchType func(string) bool
}

// Module enables ID masking for the app. Install it once, anywhere in
// the option list — it takes effect before the GraphQL schema is built,
// so masked fields are declared as the MaskedID scalar rather than Int.
func Module(cfg Config) nexus.Option {
	codec := cfg.Codec
	if codec == nil {
		key, err := resolveKey(cfg.Key)
		if err != nil {
			return nexus.Error(err)
		}
		codec = NewAESCodec(key)
	}

	p := &policy{
		codec:     codec,
		match:     cfg.Match,
		incl:      keySet(cfg.Include),
		excl:      keySet(cfg.Exclude),
		types:     typeSet(cfg.Types),
		matchType: cfg.MatchType,
	}
	if p.match == nil {
		p.match = DefaultMatch
	}

	mu.Lock()
	current = p
	mu.Unlock()

	// Installed eagerly rather than from a lifecycle hook: schema
	// construction reads maskhook.Enabled() while options are still
	// being assembled, and a hook would land after it.
	hooks := maskhook.Hooks{
		IsID:   p.isID,
		Mask:   p.mask,
		Unmask: p.unmask,
	}
	if p.excl != nil {
		hooks.Skip = p.skip
	}
	// Left nil when unscoped, so the framework skips the check entirely
	// rather than calling a predicate that always says yes.
	if p.types != nil || p.matchType != nil {
		hooks.TypeAllowed = p.typeAllowed
	}
	maskhook.Install(hooks)

	return extension.Use(extension.Plugin{
		Name:    "maskid",
		Version: "1.0.0",
		Icon:    "eye-off",
	})
}

func (p *policy) isID(key string) bool {
	if key == "" {
		return false
	}
	if p.excl[strings.ToLower(key)] {
		return false
	}
	if p.incl[strings.ToLower(key)] {
		return true
	}
	return p.match(key)
}

func (p *policy) skip(key string) bool { return p.excl[strings.ToLower(key)] }

func (p *policy) typeAllowed(name string) bool {
	if name == "" {
		return false
	}
	if p.types[name] {
		return true
	}
	return p.matchType != nil && p.matchType(name)
}

func (p *policy) mask(_ string, id int64) (string, bool) {
	return p.codec.Mask(id), true
}

func (p *policy) unmask(_ string, s string) (int64, bool) {
	return p.codec.Unmask(s)
}

// DefaultMatch is the built-in field policy: "id" and "ids" exactly, or
// any name ending in Id/ID/_id, with an optional plural s. Case matters
// on the suffix — that is what keeps "uuid", "valid" and "paid" out
// without needing a denylist of every English word ending in those two
// letters. The all-caps opaque-identifier names that would slip through
// are excluded explicitly.
func DefaultMatch(key string) bool {
	switch strings.ToLower(key) {
	case "id", "ids":
		return true
	case "uuid", "uuids", "guid", "guids", "cuid", "nanoid", "oid":
		return false
	}
	k := strings.TrimSuffix(key, "s")
	return strings.HasSuffix(k, "Id") ||
		strings.HasSuffix(k, "ID") ||
		strings.HasSuffix(k, "_id") ||
		strings.HasSuffix(k, "_ID")
}

// Mask exposes the active codec to application code — for building a
// link or an export outside the automatic transports. It returns the
// decimal id unchanged when masking isn't enabled, so call sites work
// either way.
func Mask(id int64) string {
	mu.RLock()
	p := current
	mu.RUnlock()
	if p == nil {
		return fmt.Sprint(id)
	}
	return p.codec.Mask(id)
}

// Unmask reverses Mask. ok is false for a value this process didn't
// produce — treat that as a 404, not a 500.
func Unmask(s string) (int64, bool) {
	mu.RLock()
	p := current
	mu.RUnlock()
	if p == nil {
		return 0, false
	}
	return p.codec.Unmask(s)
}

// typeSet keeps type names case-sensitive — unlike field keys, they are
// Go identifiers, and folding them would let "user" match "User".
func typeSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func keySet(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[strings.ToLower(k)] = true
	}
	return m
}

func resolveKey(key string) ([]byte, error) {
	if key == "" {
		key = os.Getenv(KeyEnv)
	}
	if key != "" {
		return []byte(key), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("maskid: no key configured and no entropy available: %w", err)
	}
	log.Printf("maskid: no key set (Config.Key or $%s) — generated a random one. "+
		"Masked IDs will change on every restart; set a key before production.", KeyEnv)
	return buf, nil
}

// --- default codec -------------------------------------------------------

// AESCodec is the default Codec: a deterministic single-block AES
// permutation. See the package doc for the construction and its
// properties.
type AESCodec struct {
	block cipher.Block
}

// NewAESCodec derives a codec from a secret of any length.
func NewAESCodec(secret []byte) *AESCodec {
	sum := sha256.Sum256(append([]byte("nexus/maskid/v1|"), secret...))
	// SHA-256 gives 32 bytes; AES-256 over one block is the same
	// permutation strength as AES-128 here and costs nothing extra.
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		// Unreachable: 32 bytes is always a valid AES key size.
		panic("maskid: " + err.Error())
	}
	return &AESCodec{block: block}
}

// maskLen is the base64url length of one 16-byte AES block.
const maskLen = 22

func (c *AESCodec) Mask(id int64) string {
	var plain [aes.BlockSize]byte
	copy(plain[:8], domainTag[:])
	binary.BigEndian.PutUint64(plain[8:], uint64(id))

	var out [aes.BlockSize]byte
	c.block.Encrypt(out[:], plain[:])
	return base64.RawURLEncoding.EncodeToString(out[:])
}

func (c *AESCodec) Unmask(s string) (int64, bool) {
	if len(s) != maskLen {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != aes.BlockSize {
		return 0, false
	}
	var plain [aes.BlockSize]byte
	c.block.Decrypt(plain[:], raw)

	// Constant-time so a caller can't probe the key by timing how far a
	// candidate string got before rejection.
	if subtle.ConstantTimeCompare(plain[:8], domainTag[:]) != 1 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(plain[8:])), true
}

// domainTag is the 8-byte constant baked into every block. It
// authenticates the mask: a random 16-byte string decrypts to a matching
// tag with probability 2⁻⁶⁴, so garbage is rejected rather than decoded
// into some unrelated record's id.
var domainTag = func() [8]byte {
	sum := sha256.Sum256([]byte("nexus/maskid/v1/tag"))
	var tag [8]byte
	copy(tag[:], sum[:8])
	return tag
}()
