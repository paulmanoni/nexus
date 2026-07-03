package auth

import (
	"context"
	"errors"
	"testing"
)

func TestModelBackendLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryUserStore()
	if _, err := store.CreateUser("alice", "s3cret-pw", "ADMIN"); err != nil {
		t.Fatal(err)
	}
	backend := NewModelBackend(store)

	// Correct credentials → identity with roles.
	id, err := Authenticate(ctx, Password{Username: "alice", Password: "s3cret-pw"}, backend)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if id.ID != "alice" || len(id.Roles) != 1 || id.Roles[0] != "ADMIN" {
		t.Fatalf("identity = %+v", id)
	}

	// Wrong password → ErrInvalidCredentials (generic).
	if _, err := Authenticate(ctx, Password{Username: "alice", Password: "nope"}, backend); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: want ErrInvalidCredentials, got %v", err)
	}
	// Unknown user → same generic error (no enumeration).
	if _, err := Authenticate(ctx, Password{Username: "ghost", Password: "x"}, backend); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: want ErrInvalidCredentials, got %v", err)
	}
}

func TestModelBackendGetUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryUserStore()
	store.CreateUser("bob", "password-bob-1")
	backend := NewModelBackend(store)

	id, err := backend.GetUser(ctx, "bob")
	if err != nil || id == nil || id.ID != "bob" {
		t.Fatalf("GetUser: id=%+v err=%v", id, err)
	}
	missing, err := backend.GetUser(ctx, "nobody")
	if err != nil || missing != nil {
		t.Fatalf("GetUser missing: id=%+v err=%v", missing, err)
	}
}

// TestModelBackendRehashOnLogin proves a password stored under a
// non-default algorithm is transparently re-encoded with the default on a
// successful login.
func TestModelBackendRehashOnLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Store with a store whose hasher is pbkdf2 (non-default), but a
	// backend whose default is bcrypt.
	pbkStore := NewMemoryUserStore(Hashers{Default: PBKDF2(), All: []Hasher{PBKDF2()}})
	pbkStore.CreateUser("carol", "carol-pw-123")

	_, encodedBefore, _ := pbkStore.ByUsername(ctx, "carol")
	if encodedBefore[:6] != "pbkdf2" {
		t.Fatalf("precondition: stored hash should be pbkdf2, got %q", encodedBefore)
	}

	backend := &ModelBackend{Store: pbkStore, Hashers: DefaultHashers()} // default bcrypt
	if _, err := backend.Authenticate(ctx, Password{Username: "carol", Password: "carol-pw-123"}); err != nil {
		t.Fatalf("login: %v", err)
	}

	_, encodedAfter, _ := pbkStore.ByUsername(ctx, "carol")
	if encodedAfter == encodedBefore {
		t.Fatal("hash was not upgraded on login")
	}
	if encodedAfter[:6] != "bcrypt" {
		t.Fatalf("upgraded hash should be bcrypt, got %q", encodedAfter)
	}
	// The upgraded hash must still verify the same password.
	if _, err := backend.Authenticate(ctx, Password{Username: "carol", Password: "carol-pw-123"}); err != nil {
		t.Fatalf("login after upgrade: %v", err)
	}
}

func TestAuthenticateTriesBackendsInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	primary := NewMemoryUserStore()
	primary.CreateUser("dave", "dave-pw-123")
	secondary := NewMemoryUserStore()
	secondary.CreateUser("erin", "erin-pw-123")

	backends := []Backend{NewModelBackend(primary), NewModelBackend(secondary)}
	// erin exists only in the second backend — the chain must reach it.
	id, err := Authenticate(ctx, Password{Username: "erin", Password: "erin-pw-123"}, backends...)
	if err != nil || id == nil || id.ID != "erin" {
		t.Fatalf("second-backend login: id=%+v err=%v", id, err)
	}
}

func TestCreateUserDuplicate(t *testing.T) {
	t.Parallel()
	store := NewMemoryUserStore()
	if _, err := store.CreateUser("sam", "sam-pw-1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("sam", "other-pw-1234"); err == nil {
		t.Fatal("duplicate username should be rejected")
	}
}
