package nexustest_test

import (
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/nexustest"
)

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type getArgs struct {
	ID string `uri:"id"`
}

// NewGetUser is a reflective REST handler: path param -> typed args -> response.
func NewGetUser(p nexus.Params[getArgs]) (*User, error) {
	return &User{ID: p.Args.ID, Name: "user-" + p.Args.ID}, nil
}

type searchArgs struct {
	Q string `graphql:"q,required"`
}

// NewSearchUsers becomes the GraphQL field "searchUsers".
func NewSearchUsers(p nexus.Params[searchArgs]) ([]*User, error) {
	return []*User{{ID: "1", Name: p.Args.Q}}, nil
}

var module = nexus.Module("users",
	nexus.AsRest("GET", "/users/:id", NewGetUser),
	nexus.AsQuery(NewSearchUsers),
)

func TestREST(t *testing.T) {
	app := nexustest.New(t, nexus.Config{}, module)

	res := app.GET("/users/42").AssertOK()
	var u User
	res.JSON(&u)
	if u.ID != "42" || u.Name != "user-42" {
		t.Fatalf("got %+v", u)
	}
}

func TestGraphQL(t *testing.T) {
	app := nexustest.New(t, nexus.Config{}, module)

	data := app.GraphQL(`query($q:String!){ searchUsers(q:$q){ id name } }`,
		map[string]any{"q": "alice"})

	rows, ok := data["searchUsers"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("searchUsers = %#v", data["searchUsers"])
	}
	first := rows[0].(map[string]any)
	if first["name"] != "alice" {
		t.Fatalf("name = %v", first["name"])
	}
}
