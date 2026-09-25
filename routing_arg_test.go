package nexus

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// nexus.Arg: scalar-taking service methods register without a wrapper — the
// option names the wire argument(s) positionally, the args struct is
// synthesized at registration, and the op name still derives from the method.

type argSvc struct{}

func (s *argSvc) FetchUser(id uint) (*probeOut, error) {
	return &probeOut{Greeting: "user-" + strconv.Itoa(int(id))}, nil
}

func (s *argSvc) DropUser(ctx context.Context, id uint) (*probeOut, error) {
	if id == 0 {
		return nil, errors.New("no such user")
	}
	return &probeOut{Greeting: "dropped-" + strconv.Itoa(int(id))}, nil
}

func (s *argSvc) MoveUser(ctx context.Context, id uint, target string, note *string) (*probeOut, error) {
	g := "moved-" + strconv.Itoa(int(id)) + "-to-" + target
	if note != nil {
		g += "-note:" + *note
	}
	return &probeOut{Greeting: g}, nil
}

func TestArgGraphQL(t *testing.T) {
	app, stop, err := InProcess(Config{},
		Supply(&argSvc{}),
		AsQuery((*argSvc).FetchUser, Arg("id")),                     // derived name: fetchUser
		AsMutation((*argSvc).DropUser, Arg("id"), Op("removeUser")), // Op override + ctx shape
		AsMutation((*argSvc).MoveUser, Arg("id", "target", "note")), // multi-arg; pointer → optional
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ fetchUser(id: 7) { greeting } }`); !strings.Contains(got, "user-7") {
		t.Fatalf("fetchUser = %s", got)
	}
	if got := postGraphQL(t, app, `mutation { removeUser(id: 3) { greeting } }`); !strings.Contains(got, "dropped-3") {
		t.Fatalf("removeUser = %s", got)
	}
	// Multi-arg: both required args bound in order; optional pointer omitted.
	if got := postGraphQL(t, app, `mutation { moveUser(id: 5, target: "hq") { greeting } }`); !strings.Contains(got, "moved-5-to-hq") {
		t.Fatalf("moveUser = %s", got)
	}
	if got := postGraphQL(t, app, `mutation { moveUser(id: 5, target: "hq", note: "vip") { greeting } }`); !strings.Contains(got, "note:vip") {
		t.Fatalf("moveUser with note = %s", got)
	}
	// The synthesized non-pointer arguments must be REQUIRED in the schema.
	if got := postGraphQL(t, app, `{ fetchUser { greeting } }`); !strings.Contains(got, "errors") {
		t.Fatalf("missing id must be a schema error, got %s", got)
	}
	if got := postGraphQL(t, app, `mutation { moveUser(id: 5) { greeting } }`); !strings.Contains(got, "errors") {
		t.Fatalf("missing required target must be a schema error, got %s", got)
	}
}

func TestArgRestPathAndQuery(t *testing.T) {
	app, stop, err := InProcess(Config{},
		Supply(&argSvc{}),
		AsRest("GET", "/users/:id", (*argSvc).FetchUser, Arg("id")),
		AsRest("POST", "/drop", (*argSvc).DropUser, Arg("id")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/users/42", nil))
	if !strings.Contains(w.Body.String(), "user-42") {
		t.Fatalf("path bind = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/drop", strings.NewReader(`{"id":9}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "dropped-9") {
		t.Fatalf("json bind = %s", w.Body.String())
	}
}

// Repeated Arg options append, equivalent to one variadic call.
func TestArgOptionsAppend(t *testing.T) {
	app, stop, err := InProcess(Config{},
		Supply(&argSvc{}),
		AsQuery((*argSvc).FetchUser, Arg("id")),
		AsMutation((*argSvc).MoveUser, Arg("id"), Arg("target"), Arg("note"), Op("relocate")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if got := postGraphQL(t, app, `mutation { relocate(id: 1, target: "x") { greeting } }`); !strings.Contains(got, "moved-1-to-x") {
		t.Fatalf("relocate = %s", got)
	}
}

func TestArgBootErrors(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
		want string
	}{
		{"too many names", AsQuery((*argSvc).FetchUser, Arg("a", "b", "c", "d")), "takes only"},
		{"names ctx", AsQuery((*argSvc).DropUser, Arg("ctx", "id")), "context.Context"},
		{"struct param", AsQuery((*probeSvc).GreetUser, Arg("id")), "register it directly"},
		{"duplicate", AsQuery((*argSvc).FetchUser, Arg("id", "id")), "duplicate"},
		{"bad name", AsQuery((*argSvc).FetchUser, Arg("user-id")), "not a valid argument name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stop, err := InProcess(Config{}, Supply(&argSvc{}), Supply(&probeSvc{}), tc.opt)
			if stop != nil {
				defer func() { _ = stop(context.Background()) }()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want boot error containing %q, got %v", tc.want, err)
			}
		})
	}
}
