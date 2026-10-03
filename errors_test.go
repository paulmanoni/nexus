package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/middleware"
)

func TestErrorModel(t *testing.T) {
	base := errors.New("row missing")
	e := Errf(NotFound, "user %d: %w", 7, base)
	wrapped := fmt.Errorf("loading: %w", e)

	if !errors.Is(wrapped, NotFound) || errors.Is(wrapped, Conflict) {
		t.Fatal("errors.Is matches the code")
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("errors.Is sees through Cause")
	}
	var got *Error
	if !errors.As(wrapped, &got) || got.Message != "user 7: row missing" {
		t.Fatalf("errors.As = %+v", got)
	}
	if CodeOf(wrapped) != NotFound || CodeOf(nil) != "" || CodeOf(NotFound) != NotFound {
		t.Fatal("CodeOf")
	}
	if Err(Forbidden, "").Error() != "forbidden" {
		t.Fatal("an empty message falls back to the code's")
	}
	if !errors.Is(fmt.Errorf("x: %w", Conflict), Conflict) {
		t.Fatal("a bare Code is an error")
	}
}

func TestErrorOfHidesUnknownOutsideDev(t *testing.T) {
	t.Setenv(dev.Env, "")
	e := ErrorOf(errors.New("pq: password authentication failed"))
	if e.Code != Internal || e.Message != "internal error" || e.Cause == nil {
		t.Fatalf("prod = %+v", e)
	}
	t.Setenv(dev.Env, "1")
	if e := ErrorOf(errors.New("pq: boom")); e.Message != "pq: boom" {
		t.Fatalf("dev = %+v", e)
	}
	if ErrorOf(nil) != nil {
		t.Fatal("nil stays nil")
	}
}

func TestInvalidAccumulator(t *testing.T) {
	e := Invalid()
	if e.Any() {
		t.Fatal("fresh accumulator must be empty")
	}
	e.Field("email", "taken").Field("email", "invalid").Global("provider down")
	if !e.Any() || len(e.Fields["email"]) != 2 || e.Fields[GlobalErrorKey][0] != "provider down" {
		t.Fatalf("Fields = %v", e.Fields)
	}
	if first := e.First(); first["email"] != "taken" {
		t.Fatalf("First = %v", first)
	}
	if e.Error() != "validation failed: _global, email" {
		t.Fatalf("Error() = %q", e.Error())
	}
	if ext := e.Extensions(); ext["code"] != "INVALID_INPUT" || ext["errors"] == nil {
		t.Fatalf("Extensions = %v", ext)
	}
}

type signupArgs struct {
	Email string `json:"email" validate:"required"`
	Name  string `json:"name" validate:"len=2|20"`
	Age   *int   `json:"age" validate:"int=18|130"`
	Role  string `json:"role" validate:"oneof=admin|user"`
	Addr  *struct {
		City string `json:"city" validate:"required"`
	} `json:"addr"`
}

type signupOut struct {
	OK bool `json:"ok"`
}

func errorsApp(t *testing.T) *App {
	t.Helper()
	signup := func(ctx context.Context, in signupArgs) (*signupOut, error) {
		switch in.Email {
		case "taken@x.io":
			return nil, Invalid().Field("email", "already taken")
		case "gone@x.io":
			return nil, Err(NotFound, "no such invite")
		case "boom@x.io":
			return nil, errors.New("pq: connection refused")
		}
		return &signupOut{OK: true}, nil
	}
	app, stop, err := InProcess(config.Runtime{},
		AsRest("POST", "/signup", signup),
		AsMutation(signup, Op("signup")),
		AsQuery(func(ctx context.Context) (*signupOut, error) { return &signupOut{OK: true}, nil }, Op("ping")),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

func postSignup(app *App, path, body string) (int, map[string]any) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestRESTErrorTable(t *testing.T) {
	t.Setenv(dev.Env, "")
	app := errorsApp(t)
	cases := []struct {
		body   string
		status int
		code   string
		msg    string
	}{
		{`{"email":"a@x.io"}`, 201, "", ""},
		{`{"email":"taken@x.io"}`, 422, "INVALID_INPUT", "validation failed: email"},
		{`{"email":"gone@x.io"}`, 404, "NOT_FOUND", "no such invite"},
		{`{"email":"boom@x.io"}`, 500, "INTERNAL", "internal error"},
		{`{"email":1}`, 422, "INVALID_INPUT", ""},
	}
	for _, c := range cases {
		status, body := postSignup(app, "/signup", c.body)
		if status != c.status || (c.code != "" && body["code"] != c.code) || (c.msg != "" && body["message"] != c.msg) {
			t.Errorf("%s → %d %v, want %d %s %q", c.body, status, body, c.status, c.code, c.msg)
		}
	}
}

func TestRESTValidatesTags(t *testing.T) {
	app := errorsApp(t)
	status, body := postSignup(app, "/signup", `{"name":"x","age":7,"role":"root","addr":{}}`)
	if status != 422 || body["code"] != "INVALID_INPUT" {
		t.Fatalf("status = %d body = %v", status, body)
	}
	fields, _ := body["errors"].(map[string]any)
	for _, f := range []string{"email", "name", "age", "role", "addr.city"} {
		if fields[f] == nil {
			t.Errorf("no message for %s in %v", f, fields)
		}
	}
	if status, _ := postSignup(app, "/signup", `{"email":"a@x.io","name":"Al","age":30,"role":"user"}`); status != 201 {
		t.Fatalf("valid args = %d", status)
	}
}

func TestGraphQLErrorTable(t *testing.T) {
	t.Setenv(dev.Env, "")
	app := errorsApp(t)
	cases := map[string]string{
		`mutation { signup(email:"taken@x.io") { ok } }`:    `"code":"INVALID_INPUT"`,
		`mutation { signup(email:"gone@x.io") { ok } }`:     `"code":"NOT_FOUND"`,
		`mutation { signup(email:"boom@x.io") { ok } }`:     `"code":"INTERNAL"`,
		`mutation { signup(email:"") { ok } }`:              `"code":"INVALID_INPUT"`,
		`mutation { signup(email:"a@x.io", age:3) { ok } }`: `"age":[`,
	}
	for q, want := range cases {
		got := postGraphQL(t, app, q)
		if !strings.Contains(got, want) {
			t.Errorf("%s → %s, want %s", q, got, want)
		}
	}
	if got := postGraphQL(t, app, `mutation { signup(email:"boom@x.io") { ok } }`); strings.Contains(got, "pq:") {
		t.Errorf("an internal message leaked: %s", got)
	}
}

func TestMiddlewareRejectionUsesTheTable(t *testing.T) {
	deny := middleware.FromHandler(middleware.NewFunc("deny", middleware.AllTransports,
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			return rc.Reject(429, errors.New("slow down"))
		}))
	app, stop, err := InProcess(config.Runtime{},
		AsRest("GET", "/x", func(ctx context.Context) (*signupOut, error) { return &signupOut{}, nil }, Use(deny)),
		AsQuery(func(ctx context.Context) (*signupOut, error) { return &signupOut{}, nil }, Op("x"), Use(deny)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"TOO_MANY_REQUESTS"`) || !strings.Contains(w.Body.String(), "slow down") {
		t.Fatalf("REST = %d %s", w.Code, w.Body.String())
	}
	if got := postGraphQL(t, app, `{ x { ok } }`); !strings.Contains(got, `"code":"TOO_MANY_REQUESTS"`) || !strings.Contains(got, "slow down") {
		t.Fatalf("GraphQL = %s", got)
	}
}

func TestEnvelopeReceivesError(t *testing.T) {
	t.Setenv(dev.Env, "")
	var seen *Error
	wrap := func(v *signupOut, err error) (*signupOut, error) {
		seen, _ = err.(*Error)
		return &signupOut{}, nil
	}
	app, stop, err := InProcess(config.Runtime{},
		AsRest("GET", "/e", func(ctx context.Context) (*signupOut, error) { return nil, errors.New("secret") }, Envelope(wrap)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/e", nil))
	if seen == nil || seen.Code != Internal || seen.Message != "internal error" || seen.Cause.Error() != "secret" {
		t.Fatalf("envelope saw %+v", seen)
	}
}

func TestWSValidationError(t *testing.T) {
	type msg struct {
		Text string `json:"text" validate:"required"`
	}
	app, stop, err := InProcess(config.Runtime{},
		AsWS("/ws", "say", func(sess *WSSession, p Params[msg]) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	ts := httptest.NewServer(app)
	defer ts.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil { // connection.established
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"say","data":{}}`)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); !strings.Contains(s, `"code":"INVALID_INPUT"`) || !strings.Contains(s, `"text":["required"]`) {
		t.Fatalf("ws error = %s", s)
	}
}
