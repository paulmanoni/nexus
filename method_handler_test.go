package nexus

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Service-method handlers: AsQuery/AsMutation/AsRest accept a method
// expression ((*Svc).Method) or a bound method value (svc.Method) directly —
// the receiver is a DI-injected dep, ctx fills from the request, a trailing
// struct param is the args container, and the op name derives from the
// method name. This is the wrapper-free registration shape; these tests pin
// it as a supported contract, not an accident of the reflector.

type probeSvc struct{ prefix string }

type probeArgs struct {
	Name string `json:"name" query:"name"`
}

type probeOut struct {
	Greeting string `json:"greeting"`
}

func (s *probeSvc) GreetUser(ctx context.Context, a probeArgs) (*probeOut, error) {
	return &probeOut{Greeting: s.prefix + a.Name}, nil
}

func (s *probeSvc) RenameUser(ctx context.Context, a probeArgs) (*probeOut, error) {
	return &probeOut{Greeting: "renamed " + a.Name}, nil
}

// ListGreetings has no args struct at all — just the receiver and ctx.
func (s *probeSvc) ListGreetings(ctx context.Context) ([]probeOut, error) {
	return []probeOut{{Greeting: s.prefix + "all"}}, nil
}

func postGraphQL(t *testing.T, app *App, query string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":`+jsonStr(query)+`}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	return w.Body.String()
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestMethodExpressionHandlers(t *testing.T) {
	app, stop, err := InProcess(Config{},
		Supply(&probeSvc{prefix: "hi "}),
		AsQuery((*probeSvc).GreetUser),
		AsQuery((*probeSvc).ListGreetings),
		AsMutation((*probeSvc).RenameUser),
		AsRest("GET", "/greet", (*probeSvc).GreetUser),
		AsRest("POST", "/rename", (*probeSvc).RenameUser),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ greetUser(name: "ada") { greeting } }`); !strings.Contains(got, "hi ada") {
		t.Fatalf("greetUser = %s", got)
	}
	if got := postGraphQL(t, app, `{ listGreetings { greeting } }`); !strings.Contains(got, "hi all") {
		t.Fatalf("listGreetings = %s", got)
	}
	if got := postGraphQL(t, app, `mutation { renameUser(name: "bo") { greeting } }`); !strings.Contains(got, "renamed bo") {
		t.Fatalf("renameUser = %s", got)
	}

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/greet?name=zed", nil))
	if !strings.Contains(w.Body.String(), "hi zed") {
		t.Fatalf("GET /greet = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/rename", strings.NewReader(`{"name":"kim"}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "renamed kim") {
		t.Fatalf("POST /rename = %s", w.Body.String())
	}
}

// A bound method value must register under the same op name as the method
// expression — the runtime decorates it "GreetUser-fm" and the name
// extraction strips the wrapper suffix.
func TestBoundMethodValueHandler(t *testing.T) {
	svc := &probeSvc{prefix: "yo "}
	app, stop, err := InProcess(Config{},
		AsQuery(svc.GreetUser),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ greetUser(name: "lin") { greeting } }`); !strings.Contains(got, "yo lin") {
		t.Fatalf("bound greetUser = %s", got)
	}
}

// The plain free-function shape — ctx first, args struct last, no Params[T]
// — is the same contract the method forms ride on.
func plainGreet(ctx context.Context, a probeArgs) (*probeOut, error) {
	return &probeOut{Greeting: "plain " + a.Name}, nil
}

func TestPlainFuncHandler(t *testing.T) {
	app, stop, err := InProcess(Config{},
		AsQuery(plainGreet),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ plainGreet(name: "mia") { greeting } }`); !strings.Contains(got, "plain mia") {
		t.Fatalf("plainGreet = %s", got)
	}
}
