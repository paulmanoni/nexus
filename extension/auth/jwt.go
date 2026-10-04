package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A jwt scheme verifies tokens another service issued — HS256 with a
// shared secret, RS256 or ES256 with a public key or the issuer's JWKS —
// and loads the user its subject claim names. nexus never issues them.

// jwtVerifier checks one scheme's tokens.
type jwtVerifier struct {
	sc     SchemeSettings
	secret []byte
	key    crypto.PublicKey // a PEM public_key
	jwks   *jwksKeys
}

func newJWTVerifier(sc SchemeSettings) (*jwtVerifier, error) {
	v := &jwtVerifier{sc: sc}
	switch {
	case sc.Secret != "":
		v.secret = []byte(sc.Secret)
	case sc.PublicKey != "":
		block, _ := pem.Decode([]byte(sc.PublicKey))
		if block == nil {
			return nil, errors.New("public_key is not PEM")
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("public_key: %w", err)
		}
		v.key = k
	case sc.JWKS != "":
		v.jwks = &jwksKeys{url: sc.JWKS, keys: map[string]crypto.PublicKey{}}
	}
	return v, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// verify returns the token's subject, or why it isn't accepted.
func (v *jwtVerifier) verify(ctx context.Context, tok string) (string, string) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", "malformed token"
	}
	var h jwtHeader
	if !decodeSegment(parts[0], &h) {
		return "", "malformed token"
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "malformed token"
	}
	signed := []byte(parts[0] + "." + parts[1])
	if reason := v.checkSignature(ctx, h, signed, sig); reason != "" {
		return "", reason
	}
	var claims map[string]any
	if !decodeSegment(parts[1], &claims) {
		return "", "malformed token"
	}
	now := time.Now()
	if exp, ok := numericClaim(claims, "exp"); ok && now.After(exp.Add(v.sc.Leeway)) {
		return "", "token expired"
	}
	if nbf, ok := numericClaim(claims, "nbf"); ok && now.Add(v.sc.Leeway).Before(nbf) {
		return "", "token not valid yet"
	}
	if v.sc.Issuer != "" && claims["iss"] != v.sc.Issuer {
		return "", "wrong issuer"
	}
	if v.sc.Audience != "" && !hasAudience(claims["aud"], v.sc.Audience) {
		return "", "wrong audience"
	}
	switch sub := claims[v.sc.Subject].(type) {
	case string:
		if sub != "" {
			return sub, ""
		}
	case float64:
		return strconv.FormatFloat(sub, 'f', -1, 64), ""
	}
	return "", "no " + v.sc.Subject + " claim"
}

// checkSignature accepts only the algorithms the configured key is for, so
// a token can't choose HS256 against an RSA key (or "none").
func (v *jwtVerifier) checkSignature(ctx context.Context, h jwtHeader, signed, sig []byte) string {
	if v.secret != nil {
		if h.Alg != "HS256" {
			return "algorithm " + h.Alg + " not accepted"
		}
		mac := hmac.New(sha256.New, v.secret)
		mac.Write(signed)
		if !hmac.Equal(mac.Sum(nil), sig) {
			return "bad signature"
		}
		return ""
	}
	key := v.key
	if v.jwks != nil {
		var err error
		if key, err = v.jwks.get(ctx, h.Kid); err != nil {
			return err.Error()
		}
	}
	digest := sha256.Sum256(signed)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if h.Alg != "RS256" {
			return "algorithm " + h.Alg + " not accepted"
		}
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) != nil {
			return "bad signature"
		}
	case *ecdsa.PublicKey:
		if h.Alg != "ES256" || len(sig) != 64 {
			return "algorithm " + h.Alg + " not accepted"
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, digest[:], r, s) {
			return "bad signature"
		}
	default:
		return "unsupported key"
	}
	return ""
}

func decodeSegment(seg string, out any) bool {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	return err == nil && json.Unmarshal(b, out) == nil
}

func numericClaim(c map[string]any, name string) (time.Time, bool) {
	f, ok := c[name].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func hasAudience(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if x == want {
				return true
			}
		}
	}
	return false
}

// jwksKeys fetches and caches an issuer's keys, refetching (at most once a
// minute) when a token names a key id it doesn't know.
type jwksKeys struct {
	url     string
	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

func (j *jwksKeys) get(ctx context.Context, kid string) (crypto.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	stale := time.Since(j.fetched) > time.Hour
	if k, ok := j.keys[kid]; ok && !stale {
		return k, nil
	}
	if time.Since(j.fetched) > time.Minute || stale {
		if err := j.fetch(ctx); err != nil {
			return nil, fmt.Errorf("jwks: %w", err)
		}
	}
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(j.keys) == 1 {
		for _, k := range j.keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("unknown key %q", kid)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j *jwksKeys) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", j.url, res.Status)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&set); err != nil {
		return err
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if pk := k.publicKey(); pk != nil {
			keys[k.Kid] = pk
		}
	}
	j.keys, j.fetched = keys, time.Now()
	return nil
}

func (k jwk) publicKey() crypto.PublicKey {
	b := func(s string) *big.Int {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
		return new(big.Int).SetBytes(raw)
	}
	switch k.Kty {
	case "RSA":
		n, e := b(k.N), b(k.E)
		if n == nil || e == nil {
			return nil
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}
	case "EC":
		x, y := b(k.X), b(k.Y)
		if k.Crv != "P-256" || x == nil || y == nil {
			return nil
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	}
	return nil
}
