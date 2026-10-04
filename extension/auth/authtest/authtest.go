// Package authtest helps tests of apps using extension/auth: requests
// authenticated as anyone, and an in-memory Users.
//
//	app := nexustest.New(t, config.Runtime{}, auth.Module(auth.Config{Users: auth.StaticUsers(users)}), orders.Module)
//
//	staff := app.With(authtest.As(&auth.Identity{ID: "7", Kind: "staff", Perms: []string{"orders.*"}}))
//	staff.GET("/admin/orders").AssertOK()
//	app.With(authtest.AsUser("7")).GET("/me").AssertOK()   // loaded through the app's Users
//
// The credential works only in a test binary: a production build ignores it.
package authtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"github.com/paulmanoni/nexus/v2/extension/auth"
)

// As returns request headers authenticating as id — every gate, area and
// policy then runs as for a real sign-in. Use them with nexustest's
// App.With, or on any request.
func As(id *auth.Identity) http.Header {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := hex.EncodeToString(b)
	auth.RegisterTestIdentity(key, id)
	return http.Header{auth.TestIdentityHeader: {key}}
}

// AsUser returns request headers authenticating as user id, loaded through
// the app's Users (Config.Users) — what a session or token for them gives.
func AsUser(id string) http.Header { return http.Header{auth.TestUserHeader: {id}} }

// Users is an in-memory auth.Users for tests and examples: logins are
// case-insensitive, passwords hashed with a fast bcrypt.
type Users struct {
	mu      sync.Mutex
	byID    map[string]*user
	hashers auth.Hashers
}

type user struct {
	id    auth.Identity
	login string
	hash  string
}

// NewUsers returns an empty Users.
func NewUsers() *Users {
	bc := auth.BCryptCost(4)
	return &Users{byID: map[string]*user{}, hashers: auth.Hashers{Default: bc, All: []auth.Hasher{bc}}}
}

// Add stores a user with login and password; id.ID names it.
func (u *Users) Add(id auth.Identity, login, password string) *Users {
	h, err := u.hashers.Hash(password)
	if err != nil {
		panic(err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.byID[id.ID] = &user{id: id, login: strings.ToLower(login), hash: h}
	return u
}

// FindLogin implements auth.Users.
func (u *Users) FindLogin(_ context.Context, login string) (*auth.Identity, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range u.byID {
		if x.login == strings.ToLower(login) {
			id := x.id
			return &id, x.hash, nil
		}
	}
	return nil, "", nil
}

// Load implements auth.Users.
func (u *Users) Load(_ context.Context, id string) (*auth.Identity, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x, ok := u.byID[id]; ok {
		cp := x.id
		return &cp, nil
	}
	return nil, nil
}

// SetPassword implements auth.PasswordSetter.
func (u *Users) SetPassword(_ context.Context, id, encoded string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x, ok := u.byID[id]; ok {
		x.hash = encoded
	}
	return nil
}

// Remove deletes a user: their credentials stop working once the [auth]
// cache no longer holds them (at once with cache = "-1s", or auth.Refresh).
func (u *Users) Remove(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.byID, id)
}
