package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2/resource"
)

// StoredToken is what a TokenStore keeps for an issued bearer token or API
// key: whose it is, which scheme issued it, and when it expires (zero for
// none). The token itself is never stored — only its SHA-256.
type StoredToken struct {
	UserID  string    `json:"user_id"`
	Scheme  string    `json:"scheme"`
	Expires time.Time `json:"expires,omitzero"`
}

// TokenStore keeps issued tokens by hash. Config.Tokens sets it; the
// default is NewMemoryTokenStore, which loses every token on restart and
// isn't shared between replicas — use CacheTokens (Redis through
// extension/cache) or your own store in production.
type TokenStore interface {
	Save(ctx context.Context, hash string, t StoredToken, ttl time.Duration) error
	// Load returns nil, nil for a hash it doesn't hold.
	Load(ctx context.Context, hash string) (*StoredToken, error)
	Delete(ctx context.Context, hash string) error
}

// MemoryTokenStore is an in-process TokenStore. It survives nexus dev
// rebuilds (dev state), so a sign-in outlives a save.
type MemoryTokenStore struct {
	mu sync.Mutex
	m  map[string]StoredToken
}

func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{m: map[string]StoredToken{}} }

func (s *MemoryTokenStore) Save(_ context.Context, hash string, t StoredToken, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[hash] = t
	return nil
}

func (s *MemoryTokenStore) Load(_ context.Context, hash string) (*StoredToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[hash]
	if !ok {
		return nil, nil
	}
	if !t.Expires.IsZero() && time.Now().After(t.Expires) {
		delete(s.m, hash)
		return nil, nil
	}
	return &t, nil
}

func (s *MemoryTokenStore) Delete(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, hash)
	return nil
}

func (s *MemoryTokenStore) SnapshotDev() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(s.m)
}

func (s *MemoryTokenStore) RestoreDev(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(b, &s.m)
}

// CacheTokens keeps tokens in a cache — Redis when extension/cache links
// it, so tokens survive restarts and are shared between replicas.
func CacheTokens(c resource.Cache) TokenStore { return cacheTokens{c} }

type cacheTokens struct{ c resource.Cache }

const tokenKeyPrefix = "auth:token:"

func (s cacheTokens) Save(ctx context.Context, hash string, t StoredToken, ttl time.Duration) error {
	return s.c.Set(ctx, tokenKeyPrefix+hash, t, ttl)
}

func (s cacheTokens) Load(ctx context.Context, hash string) (*StoredToken, error) {
	var t StoredToken
	if err := s.c.Get(ctx, tokenKeyPrefix+hash, &t); err != nil {
		// resource.Cache reports a miss as an error; an unknown token is
		// the answer either way.
		return nil, nil //nolint:nilerr
	}
	if t.UserID == "" || !t.Expires.IsZero() && time.Now().After(t.Expires) {
		return nil, nil
	}
	return &t, nil
}

func (s cacheTokens) Delete(ctx context.Context, hash string) error {
	return s.c.Delete(ctx, tokenKeyPrefix+hash)
}

// newToken is 256 random bits, base64url.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
