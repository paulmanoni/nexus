package auth

import (
	"context"
	"testing"
)

// A dev store that survives a rebuild has to come back with users that still
// authenticate — the whole point is not re-seeding them after every save.
func TestMemoryUserStoreDevStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	first := NewMemoryUserStore()
	if _, err := first.CreateUser("alice", "s3cret-pw", "ADMIN"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	blob, err := first.SnapshotDev()
	if err != nil {
		t.Fatalf("SnapshotDev: %v", err)
	}

	second := NewMemoryUserStore()
	if err := second.RestoreDev(blob); err != nil {
		t.Fatalf("RestoreDev: %v", err)
	}

	id, err := Authenticate(ctx, Password{Username: "alice", Password: "s3cret-pw"},
		NewModelBackend(second))
	if err != nil {
		t.Fatalf("restored user does not authenticate: %v", err)
	}
	if id.ID != "alice" || len(id.Roles) != 1 || id.Roles[0] != "ADMIN" {
		t.Errorf("restored identity = %+v, want alice/[ADMIN]", id)
	}
	if _, err := Authenticate(ctx, Password{Username: "alice", Password: "wrong"},
		NewModelBackend(second)); err != ErrInvalidCredentials {
		t.Errorf("wrong password accepted after restore: %v", err)
	}
	// Fresh users keep working alongside restored ones.
	if _, err := second.CreateUser("bob", "another-pw"); err != nil {
		t.Errorf("CreateUser after restore: %v", err)
	}
}

// A user the new process seeded itself wins over the snapshot: its password
// and roles are what the current code says they are.
func TestMemoryUserStoreDevStateKeepsSeededUsers(t *testing.T) {
	ctx := context.Background()
	old := NewMemoryUserStore()
	if _, err := old.CreateUser("alice", "old-pw", "USER"); err != nil {
		t.Fatal(err)
	}
	blob, err := old.SnapshotDev()
	if err != nil {
		t.Fatal(err)
	}

	fresh := NewMemoryUserStore()
	if _, err := fresh.CreateUser("alice", "new-pw", "ADMIN"); err != nil {
		t.Fatal(err)
	}
	if err := fresh.RestoreDev(blob); err != nil {
		t.Fatalf("RestoreDev: %v", err)
	}
	id, err := Authenticate(ctx, Password{Username: "alice", Password: "new-pw"},
		NewModelBackend(fresh))
	if err != nil {
		t.Fatalf("seeded user was overwritten by the snapshot: %v", err)
	}
	if len(id.Roles) != 1 || id.Roles[0] != "ADMIN" {
		t.Errorf("roles = %v, want [ADMIN]", id.Roles)
	}
}

func TestMemoryUserStoreDevStateRejectsGarbage(t *testing.T) {
	s := NewMemoryUserStore()
	if err := s.RestoreDev([]byte("not json")); err == nil {
		t.Error("expected an error for a corrupt snapshot")
	}
}
