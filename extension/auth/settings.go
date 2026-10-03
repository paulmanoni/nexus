package auth

import (
	"fmt"
	"sort"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
)

// Settings is the [auth] table of nexus.toml, read by the config-driven
// path (Config.Users). Config.Settings sets it in Go instead.
//
//	[auth]
//	default = "signed-in"     # every endpoint needs a sign-in unless it is Public; "public" opts out
//	cache   = "5m"            # Users.Load results kept per user id; a negative value ("-1s") turns it off
//
//	[auth.schemes.web]
//	type   = "session"        # cookie → server-side session → user id
//
//	[auth.schemes.api]
//	type = "bearer"           # opaque tokens auth.SignIn issues, stored hashed
//	ttl  = "12h"
//
//	[auth.passwords]
//	hashers    = ["bcrypt", "argon2id", "pbkdf2"]   # the first hashes new passwords
//	min_length = 8
type Settings struct {
	Default   string                    `toml:"default"`
	Cache     time.Duration             `toml:"cache"`
	Schemes   map[string]SchemeSettings `toml:"schemes"`
	Passwords PasswordSettings          `toml:"passwords"`
}

// SchemeSettings is one [auth.schemes.<name>] table.
type SchemeSettings struct {
	// Type is "session", "bearer" or "apikey".
	Type string `toml:"type"`
	// TTL is a bearer token's lifetime (default 12h), or the session's
	// (default extension/session's 14 days). API keys don't expire.
	TTL time.Duration `toml:"ttl"`
	// Cookie names the session cookie (default extension/session's).
	Cookie string `toml:"cookie"`
	// Secure marks the session cookie Secure; set it behind TLS.
	Secure bool `toml:"secure"`
	// Header is where an API key arrives (default "X-API-Key").
	Header string `toml:"header"`
}

// PasswordSettings is [auth.passwords]: how auth.Login verifies and
// auth.SetPassword hashes and validates.
type PasswordSettings struct {
	// Hashers lists algorithms: "bcrypt", "argon2id", "pbkdf2". The first
	// hashes new passwords; all of them verify, and a password stored with
	// another is rehashed on its next sign-in.
	Hashers []string `toml:"hashers"`
	// MinLength is the shortest password SetPassword accepts (default 8).
	MinLength int `toml:"min_length"`
	// Common, Numeric and Similar refuse a common password, an all-digit
	// one, and one like the user's id or login. All default to true.
	Common  *bool `toml:"common"`
	Numeric *bool `toml:"numeric"`
	Similar *bool `toml:"similar"`
}

const (
	SchemeSession = "session"
	SchemeBearer  = "bearer"
	SchemeAPIKey  = "apikey"

	defaultBearerTTL = 12 * time.Hour
	defaultCacheTTL  = 5 * time.Minute
)

var settingsSection = config.Section[Settings]("auth", Settings{Default: "signed-in", Cache: defaultCacheTTL})

// resolvedSettings is Settings checked and filled with defaults.
type resolvedSettings struct {
	public    bool
	cache     time.Duration
	schemes   []namedScheme // in the order they are tried
	hashers   Hashers
	validator []PasswordValidator
}

type namedScheme struct {
	name string
	SchemeSettings
}

func resolveSettings(s Settings) (*resolvedSettings, error) {
	r := &resolvedSettings{cache: s.Cache}
	if r.cache == 0 {
		r.cache = defaultCacheTTL
	}
	switch s.Default {
	case "", "signed-in":
	case "public":
		r.public = true
	default:
		return nil, fmt.Errorf(`[auth] default = %q: want "signed-in" or "public"`, s.Default)
	}
	if len(s.Schemes) == 0 {
		s.Schemes = map[string]SchemeSettings{"web": {Type: SchemeSession}}
	}
	for name, sc := range s.Schemes {
		switch sc.Type {
		case SchemeSession:
		case SchemeBearer:
			if sc.TTL <= 0 {
				sc.TTL = defaultBearerTTL
			}
		case SchemeAPIKey:
			if sc.Header == "" {
				sc.Header = "X-API-Key"
			}
		default:
			return nil, fmt.Errorf(`[auth.schemes.%s] type = %q: want "session", "bearer" or "apikey"`, name, sc.Type)
		}
		r.schemes = append(r.schemes, namedScheme{name, sc})
	}
	// Explicit credentials before the ambient cookie: an API key, then a
	// bearer token, then the session; by name within a type.
	rank := map[string]int{SchemeAPIKey: 0, SchemeBearer: 1, SchemeSession: 2}
	sort.Slice(r.schemes, func(i, j int) bool {
		a, b := r.schemes[i], r.schemes[j]
		if rank[a.Type] != rank[b.Type] {
			return rank[a.Type] < rank[b.Type]
		}
		return a.name < b.name
	})

	names := s.Passwords.Hashers
	if len(names) == 0 {
		r.hashers = DefaultHashers()
	} else {
		for _, n := range names {
			h, ok := map[string]func() Hasher{"bcrypt": BCrypt, "argon2id": Argon2id, "pbkdf2": PBKDF2}[n]
			if !ok {
				return nil, fmt.Errorf(`[auth.passwords] hashers: unknown %q (want bcrypt, argon2id or pbkdf2)`, n)
			}
			r.hashers.All = append(r.hashers.All, h())
		}
		r.hashers.Default = r.hashers.All[0]
	}
	minLen := s.Passwords.MinLength
	if minLen <= 0 {
		minLen = 8
	}
	r.validator = []PasswordValidator{MinLength(minLen)}
	on := func(b *bool) bool { return b == nil || *b }
	if on(s.Passwords.Common) {
		r.validator = append(r.validator, NotCommon())
	}
	if on(s.Passwords.Numeric) {
		r.validator = append(r.validator, NotNumericOnly())
	}
	if on(s.Passwords.Similar) {
		r.validator = append(r.validator, NotSimilarToUser())
	}
	return r, nil
}

func (r *resolvedSettings) scheme(name string) (namedScheme, bool) {
	for _, s := range r.schemes {
		if s.name == name {
			return s, true
		}
	}
	return namedScheme{}, false
}

// firstOf returns the first scheme of type t.
func (r *resolvedSettings) firstOf(t string) (namedScheme, bool) {
	for _, s := range r.schemes {
		if s.Type == t {
			return s, true
		}
	}
	return namedScheme{}, false
}
