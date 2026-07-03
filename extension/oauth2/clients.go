package oauth2

import (
	"context"
	"errors"
	"strings"
	"sync"

	oauth2lib "github.com/go-oauth2/oauth2/v4"
	"golang.org/x/crypto/bcrypt"
)

// StaticClient is the data shape used by NewStaticClientStore.
type StaticClient struct {
	ID, Secret, Domain, UserID string
	Public                     bool
}

type staticClient struct{ c StaticClient }

func (s *staticClient) GetID() string     { return s.c.ID }
func (s *staticClient) GetSecret() string { return s.c.Secret }
func (s *staticClient) GetDomain() string { return s.c.Domain }
func (s *staticClient) GetUserID() string { return s.c.UserID }
func (s *staticClient) IsPublic() bool    { return s.c.Public || s.c.Secret == "" }

// VerifyPassword backs go-oauth2's password-protected-client check.
// Same secret-format matrix as VerifyBcrypt — supports raw bcrypt
// and {bcrypt}-prefixed hashes plus literal compare for tests.
func (s *staticClient) VerifyPassword(input string) bool {
	return verifyClientSecret(s.c.Secret, input)
}

// NewStaticClientStore returns a ClientStore over an in-memory list.
// Useful for tests and tiny apps; for production, prefer
// NewLoaderClientStore against your DB.
func NewStaticClientStore(clients ...StaticClient) oauth2lib.ClientStore {
	m := make(map[string]*staticClient, len(clients))
	for _, c := range clients {
		c := c
		m[c.ID] = &staticClient{c: c}
	}
	return &staticStore{clients: m}
}

type staticStore struct {
	mu      sync.RWMutex
	clients map[string]*staticClient
}

func (s *staticStore) GetByID(_ context.Context, id string) (oauth2lib.ClientInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[id]
	if !ok {
		return nil, errors.New("oauth2: client not found")
	}
	return c, nil
}

// LoaderFunc loads a client record by ID. Implementations typically
// hit a DB or cache; the result must satisfy oauth2lib.ClientInfo.
// Wrap a pure-data record with NewLoadedClient when you only have
// the fields, not the interface.
type LoaderFunc func(ctx context.Context, id string) (oauth2lib.ClientInfo, error)

// NewLoaderClientStore wraps a LoaderFunc as a ClientStore. The
// loader is called on every GetByID — add caching inside the
// loader if your backend can't take the load.
func NewLoaderClientStore(load LoaderFunc) oauth2lib.ClientStore {
	return &loaderStore{load: load}
}

type loaderStore struct{ load LoaderFunc }

func (s *loaderStore) GetByID(ctx context.Context, id string) (oauth2lib.ClientInfo, error) {
	if s.load == nil {
		return nil, errors.New("oauth2: loader not configured")
	}
	return s.load(ctx, id)
}

// NewLoadedClient adapts a plain StaticClient into a ClientInfo —
// useful inside a LoaderFunc when you're reading rows from a DB and
// want the same secret-verification matrix as the static store.
func NewLoadedClient(c StaticClient) oauth2lib.ClientInfo {
	return &staticClient{c: c}
}

// verifyClientSecret implements the secret-format matrix used by
// the bundled clients: {bcrypt}-prefixed, raw bcrypt, and plain
// equality (for tests). Public clients (empty stored secret) accept
// only an empty input.
func verifyClientSecret(stored, input string) bool {
	stored = strings.TrimPrefix(stored, "{bcrypt}")
	if stored == "" {
		return input == ""
	}
	if strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") || strings.HasPrefix(stored, "$2y$") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(input)) == nil
	}
	return stored == input
}
