package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	oauth2lib "github.com/go-oauth2/oauth2/v4"
	"github.com/go-oauth2/oauth2/v4/models"
	"github.com/google/uuid"
)

// Cache is the small surface NewCacheTokenStore needs from a
// caller's cache layer. Any Redis / Memcached / in-memory wrapper
// that exposes string Get/Set/Delete with TTL fits — the package
// doesn't import a specific cache library.
type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// ErrCacheMiss is the sentinel implementations may return from Get
// when the key is absent. Treated identically to a non-nil error
// other than this one (i.e. "no token here") — tokens.go converts
// any miss/error into a nil TokenInfo so a misconfigured cache
// fails closed instead of crashing.
var ErrCacheMiss = errors.New("oauth2: cache miss")

// NewCacheTokenStore returns an oauth2lib.TokenStore that persists
// tokens in the supplied Cache under keys prefixed with `prefix`.
//
// Storage layout (4 keyspaces under prefix):
//
//	{prefix}basic:{uuid}      → JSON-encoded TokenInfo (the source of truth)
//	{prefix}access:{access}   → uuid pointing at the basic record
//	{prefix}refresh:{refresh} → uuid pointing at the basic record
//	{prefix}code:{code}       → JSON-encoded TokenInfo (auth-code grant)
//
// Refreshing rewrites access/refresh keys but reuses the basic
// record so a refresh-then-revoke-old-access doesn't strand the
// refresh token.
func NewCacheTokenStore(cache Cache, prefix string) oauth2lib.TokenStore {
	return &cacheStore{cache: cache, prefix: prefix}
}

type cacheStore struct {
	cache  Cache
	prefix string
}

func (s *cacheStore) keyBasic(id string) string  { return s.prefix + "basic:" + id }
func (s *cacheStore) keyCode(c string) string    { return s.prefix + "code:" + c }
func (s *cacheStore) keyAccess(t string) string  { return s.prefix + "access:" + t }
func (s *cacheStore) keyRefresh(t string) string { return s.prefix + "refresh:" + t }

func (s *cacheStore) Create(ctx context.Context, info oauth2lib.TokenInfo) error {
	if ext, ok := info.(oauth2lib.ExtendableTokenInfo); ok {
		values := ext.GetExtension()
		if values == nil {
			values = url.Values{}
		}
		if values.Get("jti") == "" {
			values.Set("jti", uuid.NewString())
			ext.SetExtension(values)
		}
	}
	jv, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if code := info.GetCode(); code != "" {
		return s.cache.Set(ctx, s.keyCode(code), string(jv), info.GetCodeExpiresIn())
	}

	basicID := uuid.NewString()
	now := time.Now()
	accessExp := info.GetAccessExpiresIn()
	refreshExp := accessExp

	if refresh := info.GetRefresh(); refresh != "" {
		refreshExp = info.GetRefreshCreateAt().Add(info.GetRefreshExpiresIn()).Sub(now)
		if accessExp > refreshExp {
			accessExp = refreshExp
		}
		if err := s.cache.Set(ctx, s.keyRefresh(refresh), basicID, refreshExp); err != nil {
			return err
		}
	}

	if err := s.cache.Set(ctx, s.keyBasic(basicID), string(jv), refreshExp); err != nil {
		return err
	}
	if access := info.GetAccess(); access != "" {
		if err := s.cache.Set(ctx, s.keyAccess(access), basicID, accessExp); err != nil {
			return err
		}
	}
	return nil
}

func (s *cacheStore) RemoveByCode(ctx context.Context, code string) error {
	return s.cache.Delete(ctx, s.keyCode(code))
}

func (s *cacheStore) RemoveByAccess(ctx context.Context, access string) error {
	return s.cache.Delete(ctx, s.keyAccess(access))
}

func (s *cacheStore) RemoveByRefresh(ctx context.Context, refresh string) error {
	return s.cache.Delete(ctx, s.keyRefresh(refresh))
}

func (s *cacheStore) GetByCode(ctx context.Context, code string) (oauth2lib.TokenInfo, error) {
	return s.loadToken(ctx, s.keyCode(code))
}

func (s *cacheStore) GetByAccess(ctx context.Context, access string) (oauth2lib.TokenInfo, error) {
	basicID, err := s.cache.Get(ctx, s.keyAccess(access))
	if err != nil || basicID == "" {
		return nil, nil
	}
	return s.loadToken(ctx, s.keyBasic(basicID))
}

func (s *cacheStore) GetByRefresh(ctx context.Context, refresh string) (oauth2lib.TokenInfo, error) {
	basicID, err := s.cache.Get(ctx, s.keyRefresh(refresh))
	if err != nil || basicID == "" {
		return nil, nil
	}
	return s.loadToken(ctx, s.keyBasic(basicID))
}

func (s *cacheStore) loadToken(ctx context.Context, key string) (oauth2lib.TokenInfo, error) {
	raw, err := s.cache.Get(ctx, key)
	if err != nil || raw == "" {
		return nil, nil
	}
	var tok models.Token
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}
