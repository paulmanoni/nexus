package auth

import (
	"fmt"
	"sort"
	"strings"
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
	// Login is the sign-in page an unauthenticated page visit outside every
	// area is sent to, with ?next=; empty answers 401 instead.
	Login string `toml:"login"`
	// Home is where a sign-in lands without a next, outside every area
	// (default "/").
	Home string `toml:"home"`
	// NextParam names the query and form field carrying next (default "next").
	NextParam string `toml:"next_param"`
	// Forbidden is the page a refused page visit outside every area is
	// sent to; empty answers 403.
	Forbidden string `toml:"forbidden"`
	// PageProp names the prop Inertia pages get {user, can} under (default
	// "auth"); "-" turns it off.
	PageProp      string                  `toml:"page_prop"`
	Areas         map[string]AreaSettings `toml:"areas"`
	Sessions      SessionRules            `toml:"sessions"`
	Impersonation ImpersonationSettings   `toml:"impersonation"`
	OAuth2        OAuth2Settings          `toml:"oauth2"`
	// Roles maps a role name to its permissions:
	//   [auth.roles]
	//   admin = ["*"]
	//   clerk = ["orders.view", "orders.refund"]
	// An identity's Roles add those permissions to its Perms.
	Roles map[string][]string `toml:"roles"`
	// Perms is the optional permission catalogue. When set, every
	// permission a gate (Requires, RequiresAny) or a role names must be in
	// it — a misspelling fails boot — and the dashboard lists it.
	Perms     []string         `toml:"perms"`
	Throttle  ThrottleSettings `toml:"throttle"`
	Endpoints EndpointSettings `toml:"endpoints"`
}

// AreaSettings is one [auth.areas.<name>] table: a path prefix that belongs
// to some user kinds, with its own login and landing pages.
type AreaSettings struct {
	Prefix string   `toml:"prefix"`
	Kinds  []string `toml:"kinds"` // empty: any signed-in user
	Login  string   `toml:"login"`
	Home   string   `toml:"home"`
	// Forbidden is the page a refused page visit here is sent to (a page
	// your app serves, e.g. "/admin/forbidden"); empty answers 403.
	Forbidden string `toml:"forbidden"`
}

// SessionRules is [auth.sessions]: what ends a user's sessions and tokens.
type SessionRules struct {
	// Single: signing in ends the user's other sessions and tokens.
	Single bool `toml:"single"`
	// EndOnPasswordChange: auth.SetPassword ends every other session and
	// token of the user (the one changing it stays). Default true.
	EndOnPasswordChange *bool `toml:"end_on_password_change"`
	// Idle ends a session unused this long (sessions only; 0 = never).
	Idle time.Duration `toml:"idle"`
}

// OAuth2Settings is [auth.oauth2]: the clients the token endpoint knows.
type OAuth2Settings struct {
	// RequireClient makes the password and refresh_token grants need a
	// known client too (client_credentials always does).
	RequireClient bool                      `toml:"require_client"`
	Clients       map[string]ClientSettings `toml:"clients"`
}

// ClientSettings is one [auth.oauth2.clients.<id>] table.
type ClientSettings struct {
	// Secret or SecretHash (an encoded hash, as auth.Hashers writes) is
	// what the client authenticates with; neither: a public client, which
	// can't use client_credentials.
	Secret     string `toml:"secret"`
	SecretHash string `toml:"secret_hash"`
	// Grants it may use: password, refresh_token, client_credentials
	// (empty: all three).
	Grants []string `toml:"grants"`
	// Perms and Kind (default "client") are the identity a
	// client_credentials token carries.
	Perms []string `toml:"perms"`
	Kind  string   `toml:"kind"`
}

// ImpersonationSettings is [auth.impersonation].
type ImpersonationSettings struct {
	// Permission is what a user needs to impersonate another (default
	// "auth.impersonate").
	Permission string `toml:"permission"`
	// Endpoint mounts POST {user_id} (start) and DELETE (stop) when set.
	Endpoint string `toml:"endpoint"`
}

// ThrottleSettings is [auth.throttle]: failed sign-ins allowed per account
// and per client IP in a window ("5/15m"), and how long an account stays
// locked after its limit. Empty keys use the defaults; "off" disables one.
type ThrottleSettings struct {
	Account string        `toml:"account"` // default "5/15m"
	IP      string        `toml:"ip"`      // default "50/15m"
	Lockout time.Duration `toml:"lockout"` // default 15m
}

// EndpointSettings is [auth.endpoints]: the built-in JSON endpoints, each
// mounted only when its path is set.
type EndpointSettings struct {
	Login  string `toml:"login"`  // POST {login, password, next?, scheme?}
	Logout string `toml:"logout"` // POST
	Me     string `toml:"me"`     // GET
	// Token is an OAuth2 token endpoint (RFC 6749): grant_type=password
	// and refresh_token, issuing the first bearer scheme's tokens. Revoke
	// is its revocation endpoint (RFC 7009). Both skip CSRF — they set no
	// cookie and answer only the caller.
	Token  string `toml:"token"`
	Revoke string `toml:"revoke"`
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
	// Refresh is a bearer scheme's refresh-token lifetime: SignIn then also
	// returns a refresh_token, exchanged at the token endpoint (or with
	// auth.RefreshToken) for a new pair. 0: no refresh tokens.
	Refresh time.Duration `toml:"refresh"`

	// jwt: verify tokens another service issued. One key source: Secret
	// (HS256), PublicKey (a PEM public key: RS256 or ES256) or JWKS (a
	// URL serving the issuer's keys).
	Secret    string `toml:"secret"`
	PublicKey string `toml:"public_key"`
	JWKS      string `toml:"jwks"`
	// Issuer and Audience, when set, must match the iss and aud claims.
	Issuer   string `toml:"issuer"`
	Audience string `toml:"audience"`
	// Subject names the claim holding the user id for Users.Load (default
	// "sub").
	Subject string `toml:"subject"`
	// Leeway is the clock skew allowed on exp and nbf (default 1m).
	Leeway time.Duration `toml:"leeway"`
	// oidc: "Sign in with …" through an OpenID Connect provider (Google,
	// Microsoft, Okta, Keycloak). Issuer (above) is discovered from
	// <issuer>/.well-known/openid-configuration. Login is the path that
	// starts a sign-in (?next= is kept), Redirect the callback path
	// registered with the provider (or a full URL). Claim names the claim
	// Users.FindLogin is asked with (default "email"); Scopes default to
	// openid email profile.
	ClientID     string   `toml:"client_id"`
	ClientSecret string   `toml:"client_secret"`
	Login        string   `toml:"login"`
	Redirect     string   `toml:"redirect"`
	Claim        string   `toml:"claim"`
	Scopes       []string `toml:"scopes"`

	// Revocable makes auth.RevokeUser reach this jwt scheme's tokens: one
	// issued (iat) before the user's last revocation is refused. Costs one
	// cached lookup per request; tokens then need an iat claim.
	Revocable bool `toml:"revocable"`
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
	SchemeJWT     = "jwt"
	SchemeOIDC    = "oidc"

	defaultBearerTTL = 12 * time.Hour
	defaultCacheTTL  = 5 * time.Minute
)

var settingsSection = config.Section[Settings]("auth", Settings{Default: "signed-in", Cache: defaultCacheTTL})

// resolvedSettings is Settings checked and filled with defaults.
type resolvedSettings struct {
	public        bool
	cache         time.Duration
	schemes       []namedScheme // in the order they are tried
	hashers       Hashers
	validator     []PasswordValidator
	login         string
	home          string
	nextParam     string
	forbidden     string
	pageProp      string // "" when off
	sessions      SessionRules
	endOnPw       bool
	impersonation ImpersonationSettings
	oauth2        OAuth2Settings
	roles         map[string][]string
	perms         []string
	areas         []namedArea // longest prefix first
	throttle      throttleRules
	endpoints     EndpointSettings
}

type namedArea struct {
	name string
	AreaSettings
}

type namedScheme struct {
	name string
	SchemeSettings
}

func resolveSettings(s Settings) (*resolvedSettings, error) {
	var err error
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
		case SchemeJWT:
			n := 0
			for _, v := range []string{sc.Secret, sc.PublicKey, sc.JWKS} {
				if v != "" {
					n++
				}
			}
			if n != 1 {
				return nil, fmt.Errorf(`[auth.schemes.%s] type = "jwt" needs one of secret, public_key or jwks`, name)
			}
			if sc.Subject == "" {
				sc.Subject = "sub"
			}
			if sc.Leeway <= 0 {
				sc.Leeway = time.Minute
			}
		case SchemeOIDC:
			if sc.Issuer == "" || sc.ClientID == "" || sc.Login == "" || sc.Redirect == "" {
				return nil, fmt.Errorf(`[auth.schemes.%s] type = "oidc" needs issuer, client_id, login and redirect`, name)
			}
			if sc.Claim == "" {
				sc.Claim = "email"
			}
			if len(sc.Scopes) == 0 {
				sc.Scopes = []string{"openid", "email", "profile"}
			}
			if sc.Leeway <= 0 {
				sc.Leeway = time.Minute
			}
		default:
			return nil, fmt.Errorf(`[auth.schemes.%s] type = %q: want "session", "bearer", "apikey", "jwt" or "oidc"`, name, sc.Type)
		}
		r.schemes = append(r.schemes, namedScheme{name, sc})
	}
	// Explicit credentials before the ambient cookie: an API key, then a
	// bearer token, then the session; by name within a type.
	rank := map[string]int{SchemeAPIKey: 0, SchemeJWT: 1, SchemeBearer: 2, SchemeSession: 3, SchemeOIDC: 4}
	sort.Slice(r.schemes, func(i, j int) bool {
		a, b := r.schemes[i], r.schemes[j]
		if rank[a.Type] != rank[b.Type] {
			return rank[a.Type] < rank[b.Type]
		}
		return a.name < b.name
	})

	if _, oidc := r.firstOf(SchemeOIDC); oidc {
		if _, session := r.firstOf(SchemeSession); !session {
			return nil, fmt.Errorf(`an oidc scheme signs people in with a session: add a scheme with type = "session"`)
		}
	}

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
	r.login, r.home, r.nextParam, r.endpoints = s.Login, s.Home, s.NextParam, s.Endpoints
	r.forbidden = s.Forbidden
	r.oauth2 = s.OAuth2
	r.roles, r.perms = s.Roles, s.Perms
	r.impersonation = s.Impersonation
	if r.impersonation.Permission == "" {
		r.impersonation.Permission = "auth.impersonate"
	}
	r.sessions = s.Sessions
	r.endOnPw = s.Sessions.EndOnPasswordChange == nil || *s.Sessions.EndOnPasswordChange
	switch s.PageProp {
	case "":
		r.pageProp = "auth"
	case "-":
	default:
		r.pageProp = s.PageProp
	}
	if r.home == "" {
		r.home = "/"
	}
	if r.nextParam == "" {
		r.nextParam = "next"
	}
	for name, a := range s.Areas {
		if a.Prefix == "" || a.Prefix[0] != '/' {
			return nil, fmt.Errorf(`[auth.areas.%s] prefix = %q: want a path starting with "/"`, name, a.Prefix)
		}
		if a.Home == "" {
			a.Home = a.Prefix
		}
		r.areas = append(r.areas, namedArea{name, a})
	}
	sort.Slice(r.areas, func(i, j int) bool { return len(r.areas[i].Prefix) > len(r.areas[j].Prefix) })
	if r.throttle, err = resolveThrottle(s.Throttle); err != nil {
		return nil, err
	}
	return r, nil
}

// area returns the area path belongs to: the longest prefix that is the
// path or a parent of it.
func (r *resolvedSettings) area(path string) *namedArea {
	for i := range r.areas {
		p := strings.TrimSuffix(r.areas[i].Prefix, "/")
		if path == p || strings.HasPrefix(path, p+"/") || p == "" {
			return &r.areas[i]
		}
	}
	return nil
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
