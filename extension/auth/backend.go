package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
)

// ErrInvalidCredentials is returned by Authenticate when no backend
// accepted the credentials. It is deliberately generic — never reveal
// whether the username or the password was wrong (user enumeration).
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// Credentials is what a user presents to log in. It's an open interface so
// an app can add its own kinds (an OTP, a magic-link token) that a custom
// Backend understands; Password is the built-in username/password kind.
type Credentials interface{ credentials() }

// Password is the username/password credential.
type Password struct {
	Username string
	Password string
}

func (Password) credentials() {}

// Backend authenticates a credential and re-hydrates an identity by ID —
// the Django AUTHENTICATION_BACKENDS analogue. Backends are tried in order
// by Authenticate.
type Backend interface {
	// Authenticate verifies cred. Return (identity, nil) on success,
	// (nil, nil) to defer to the next backend (wrong kind of credential,
	// or unknown user/bad password — stay silent to avoid enumeration),
	// or (nil, err) to abort the whole chain (a hard failure like a
	// locked account or a backend outage).
	Authenticate(ctx context.Context, cred Credentials) (*Identity, error)

	// GetUser loads the identity for a stable ID — used to rebuild the
	// identity from a session or token on later requests. Returns
	// (nil, nil) when the user no longer exists.
	GetUser(ctx context.Context, id string) (*Identity, error)
}

// Authenticate tries each backend in order and returns the first identity
// produced. A backend returning a hard error stops the chain. When every
// backend defers, it returns ErrInvalidCredentials.
//
//	id, err := auth.Authenticate(ctx, auth.Password{Username: u, Password: p}, backends...)
func Authenticate(ctx context.Context, cred Credentials, backends ...Backend) (*Identity, error) {
	for _, b := range backends {
		id, err := b.Authenticate(ctx, cred)
		if err != nil {
			return nil, err
		}
		if id != nil {
			return id, nil
		}
	}
	return nil, ErrInvalidCredentials
}

// UserStore is the data source a ModelBackend authenticates against — the
// seam where an app supplies its own user model (a GORM table, an external
// service, an in-memory map). Only three operations are needed for
// password login; everything else about "a user" stays in your domain.
type UserStore interface {
	// ByUsername returns the identity and its stored (encoded) password
	// for username. Return (nil, "", nil) when there's no such user —
	// ModelBackend then fails uniformly without leaking existence.
	ByUsername(ctx context.Context, username string) (id *Identity, encodedPassword string, err error)

	// ByID returns the identity for a stable ID (session re-hydration),
	// or (nil, nil) when absent.
	ByID(ctx context.Context, id string) (*Identity, error)

	// SetPassword persists a new encoded password for a user — used by the
	// transparent rehash-on-login upgrade.
	SetPassword(ctx context.Context, id, encodedPassword string) error
}

// ModelBackend is the built-in password Backend: it looks a user up in a
// UserStore and verifies the presented password with a Hashers set,
// transparently upgrading the stored hash when the algorithm/params are
// out of date. This is nexus's equivalent of Django's ModelBackend.
type ModelBackend struct {
	Store   UserStore
	Hashers Hashers
}

// NewModelBackend builds a ModelBackend, defaulting to DefaultHashers when
// none is supplied.
func NewModelBackend(store UserStore, hashers ...Hashers) *ModelBackend {
	h := DefaultHashers()
	if len(hashers) > 0 {
		h = hashers[0]
	}
	return &ModelBackend{Store: store, Hashers: h}
}

// Authenticate implements Backend for username/password credentials.
func (m *ModelBackend) Authenticate(ctx context.Context, cred Credentials) (*Identity, error) {
	pw, ok := cred.(Password)
	if !ok {
		return nil, nil // not our kind of credential
	}
	id, encoded, err := m.Store.ByUsername(ctx, pw.Username)
	if err != nil {
		return nil, err
	}
	if id == nil || encoded == "" {
		// Equalize timing against the found-user path so an attacker
		// can't distinguish "no such user" from "wrong password": hashing
		// burns roughly the same CPU as the verify we skipped.
		_, _ = m.Hashers.Hash(pw.Password)
		return nil, nil
	}
	valid, needsUpgrade, verr := m.Hashers.Verify(pw.Password, encoded)
	if verr != nil || !valid {
		return nil, nil
	}
	if needsUpgrade {
		if fresh, herr := m.Hashers.Hash(pw.Password); herr == nil {
			_ = m.Store.SetPassword(ctx, id.ID, fresh) // best-effort; login still succeeds
		}
	}
	return id, nil
}

// GetUser implements Backend.
func (m *ModelBackend) GetUser(ctx context.Context, id string) (*Identity, error) {
	return m.Store.ByID(ctx, id)
}

// --- in-memory UserStore ----------------------------------------------------

// MemoryUserStore is an in-process UserStore for tests, dev, and small
// apps. It is safe for concurrent use. Not durable — restart loses users.
type MemoryUserStore struct {
	hashers  Hashers
	mu       sync.RWMutex
	byName   map[string]*memUser
	byID     map[string]*memUser
	sequence int
}

type memUser struct {
	id       Identity
	username string
	encoded  string
}

// NewMemoryUserStore returns an empty store using the given Hashers (or
// DefaultHashers) to encode passwords passed to CreateUser.
func NewMemoryUserStore(hashers ...Hashers) *MemoryUserStore {
	h := DefaultHashers()
	if len(hashers) > 0 {
		h = hashers[0]
	}
	return &MemoryUserStore{hashers: h, byName: map[string]*memUser{}, byID: map[string]*memUser{}}
}

// CreateUser hashes password and stores a user under username, returning
// the assigned identity. roles are attached to the identity. Fails if the
// username is already taken.
func (s *MemoryUserStore) CreateUser(username, password string, roles ...string) (*Identity, error) {
	encoded, err := s.hashers.Hash(password)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byName[username]; exists {
		return nil, errors.New("auth: username already exists")
	}
	s.sequence++
	u := &memUser{
		id:       Identity{ID: username, Roles: append([]string(nil), roles...)},
		username: username,
		encoded:  encoded,
	}
	s.byName[username] = u
	s.byID[u.id.ID] = u
	return cloneIdentity(&u.id), nil
}

func (s *MemoryUserStore) ByUsername(_ context.Context, username string) (*Identity, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byName[username]
	if !ok {
		return nil, "", nil
	}
	return cloneIdentity(&u.id), u.encoded, nil
}

func (s *MemoryUserStore) ByID(_ context.Context, id string) (*Identity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return cloneIdentity(&u.id), nil
}

func (s *MemoryUserStore) SetPassword(_ context.Context, id, encoded string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return errors.New("auth: user not found")
	}
	u.encoded = encoded
	return nil
}

// SnapshotDev and RestoreDev implement nexus.DevState, so a store seeded with
// dev users survives a `nexus dev` rebuild instead of making you re-create
// them after every save. Opt in where the store is built:
//
//	store := auth.NewMemoryUserStore()
//	nexus.PreserveDev("auth.users", store)
//	if _, err := store.ByUsername(ctx, "alice"); err == nil { … }   // already there
//
// Password hashes travel as-is (they're already encoded), so restored users
// authenticate exactly as before. The hashers themselves are configuration,
// not state, and stay whatever the new process constructed.
func (s *MemoryUserStore) SnapshotDev() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := devUserSnapshot{Sequence: s.sequence, Users: make([]devUser, 0, len(s.byName))}
	for _, u := range s.byName {
		snap.Users = append(snap.Users, devUser{
			Username: u.username,
			Encoded:  u.encoded,
			Identity: u.id,
		})
	}
	sort.Slice(snap.Users, func(i, j int) bool { return snap.Users[i].Username < snap.Users[j].Username })
	return json.Marshal(snap)
}

func (s *MemoryUserStore) RestoreDev(data []byte) error {
	var snap devUserSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range snap.Users {
		if _, taken := s.byName[u.Username]; taken {
			continue // the new process seeded this user itself; it wins
		}
		stored := &memUser{id: u.Identity, username: u.Username, encoded: u.Encoded}
		s.byName[u.Username] = stored
		s.byID[stored.id.ID] = stored
	}
	if snap.Sequence > s.sequence {
		s.sequence = snap.Sequence
	}
	return nil
}

type devUserSnapshot struct {
	Sequence int       `json:"sequence"`
	Users    []devUser `json:"users"`
}

type devUser struct {
	Username string   `json:"username"`
	Encoded  string   `json:"encoded"`
	Identity Identity `json:"identity"`
}

// cloneIdentity returns a defensive copy so callers can't mutate stored
// state through the returned pointer.
func cloneIdentity(in *Identity) *Identity {
	out := *in
	out.Roles = append([]string(nil), in.Roles...)
	out.Scopes = append([]string(nil), in.Scopes...)
	return &out
}
