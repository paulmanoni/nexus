package main

import (
	"strings"
	"testing"
)

func TestMigrateOpNamesKeepsV1WireNames(t *testing.T) {
	src := []byte(`package users

import nx "github.com/paulmanoni/nexus/v2"

//nexus:query
// NewSearchUsers finds users.
func NewSearchUsers(p nx.Params[Q]) ([]User, error) { return nil, nil }

//nexus:mutation
//nexus:use nx.Op("make")
func NewCreateUser() error { return nil }

var Module = nx.Module("users",
	nx.AsQuery(NewListUsers),
	nx.AsRest("GET", "/users/:id", NewGetUser, nx.Describe("one")),
	nx.AsQuery(NewNamed, nx.Op("named")),
	nx.AsQuery(ListPets),
)
`)
	out, changes, err := migrateGoOpNames("users.go", src)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		`nx.AsQuery(NewListUsers, nx.Op("listUsers"))`,
		`nx.AsRest("GET", "/users/:id", NewGetUser, nx.Describe("one"))`,
		`nx.AsQuery(NewNamed, nx.Op("named"))`,
		`nx.AsQuery(ListPets)`,
		"//nexus:query\n//nexus:use nexus.Op(\"searchUsers\")",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, `Op("createUser")`) {
		t.Error("a handler that already names its op was given another")
	}
	if len(changes) != 2 {
		t.Errorf("changes = %d, want 2", len(changes))
	}
	if again, more, _ := migrateGoOpNames("users.go", out); len(more) != 0 || string(again) != got {
		t.Errorf("second run changed the file: %v", more)
	}
}
